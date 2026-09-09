// Package browser resolves media URLs by driving a managed Chromium instance
// and recording the requests a site's own player makes.
//
// Only network traffic is interpreted: the package never reads, rewrites or
// evaluates site JavaScript, so it does not depend on minified identifiers or
// bundle layout. Callers describe which requests they want with a predicate
// and receive the observed requests together with the headers Chromium
// actually sent, which is the minimum context needed to replay the media
// outside the browser.
//
// # Preparing the profile
//
// Origins commonly gate their watch routes behind an interactive bot check
// that no automated browser can pass. The clearance is a cookie, so it only
// has to be earned once per profile:
//
//	chromium --user-data-dir="$XDG_STATE_HOME/tarragon/anime/browser" \
//	    https://mkissa.to/anime/<showId>/p-1-sub
//
// Tick the check and wait for the player. Captures then run headless in the
// same profile. With AutoClearance enabled, Capture opens the visible window
// automatically and retries once after clearance. Otherwise it returns manual
// instructions. A check that needs more time returns ErrClearancePending while
// the window remains open; complete it and retry playback.
//
// # Known limitations
//
//   - Resolution costs a real page load. Measured against mkissa.to: about
//     2.9s cold, including Chromium start-up, and about 2.9s warm, of which
//     2s is the deliberate settle window. Next-episode prefetch and the
//     provider's stream cache are what keep this off the critical path.
//   - The clearance cookie is bound to the profile, the pinned UserAgent and,
//     in practice, the client's address. Copying the profile to another
//     machine or network does not carry it over.
//   - Only the source the page loads by itself is captured. The alternate
//     sources behind the site's player tabs are third party embeds that need
//     UI interaction and serve ads, so they are out of reach and out of
//     scope.
//   - Captured URLs carry signed, expiring tokens. They are cached briefly
//     and must be re-resolved rather than persisted.
//   - Media that a page only ever exposes as a blob: URL cannot be handed to
//     an external player at all; such requests are rejected rather than
//     returned as an unusable answer.
//   - Chromium is required. Without it stream resolution is unavailable and
//     callers must degrade to another provider.
package browser

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
)

var (
	// ErrUnavailable reports that no usable Chromium build was found.
	ErrUnavailable = errors.New("no Chromium binary found")
	// ErrChallenged reports that the origin answered the page request with a
	// bot check instead of the page. The clearance cookie lives in the
	// persistent profile, so a human has to pass the check once.
	ErrChallenged = errors.New("origin served a bot challenge")
	// ErrNoMedia reports that the page loaded but made no matching request.
	ErrNoMedia = errors.New("no media request observed")
	// ErrClosed reports use of a resolver that has been shut down.
	ErrClosed = errors.New("browser resolver is closed")
	// ErrProfileLocked reports that another process already holds the browser
	// profile. Chromium refuses to share a user data directory, and the
	// clearance cookie lives in that directory, so profiles cannot simply be
	// duplicated per process.
	ErrProfileLocked = errors.New("browser profile is already in use by another process")
)

// UserAgent is the user agent presented to the site. It is pinned rather than
// derived from the Chromium build so a captured URL keeps working when the
// header set is replayed by another client.
const UserAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36"

// windowSize must describe a desktop-sized viewport: players are commonly
// mounted lazily when they scroll into view, and a small headless window
// leaves them unmounted so no media request is ever made.
const windowSize = "1920,1080"

// Candidate is a single request observed while the page was open.
type Candidate struct {
	// URL is the fully qualified request URL.
	URL string
	// Kind is the Chromium resource type, for example "Media" or "XHR".
	Kind string
	// MIME is the response content type, empty until the response arrives.
	MIME string
	// Status is the response status, zero until the response arrives.
	Status int
	// Headers are the request headers Chromium sent, verbatim.
	Headers map[string]string
	// Frame identifies the frame that issued the request, so requests from
	// the player frame can be told apart from the host page.
	Frame string
	// Observed is the delay between navigation and the request.
	Observed time.Duration
}

// Request describes one capture.
type Request struct {
	// PageURL is the site page to open.
	PageURL string
	// Accept reports whether an observed request is wanted. It is called
	// from the event loop and must not block.
	Accept func(Candidate) bool
	// Ready optionally selects which accepted candidates start and keep the
	// settle window. Nil treats every accepted candidate as ready. Like Accept,
	// it runs in the event loop and must not block.
	Ready func(Candidate) bool
	// Settle is how long to keep observing after the first ready request
	// so alternates, such as higher quality variants or subtitle tracks, are
	// not missed. The window resets when no ready candidates remain.
	// Non-positive values use the resolver's default settle window.
	Settle time.Duration
}

// Options configure a Resolver. The zero value is usable; see New.
type Options struct {
	// Binary is the Chromium executable. Empty discovers one on PATH.
	Binary string
	// ProfileDir is a persistent user data directory. It holds the clearance
	// cookie for the origin's bot check and must not be shared with the
	// user's own browser profile.
	ProfileDir string
	// Headless runs Chromium without a visible window.
	Headless bool
	// Timeout bounds a single capture.
	Timeout time.Duration
	// IdleTimeout shuts the browser down after this long without work.
	IdleTimeout time.Duration
	// MaxSessions bounds concurrent captures.
	MaxSessions int
	// AutoClearance opens a visible browser window when the origin serves a
	// bot check, so the clearance can be earned without a terminal. Without a
	// display, the caller receives ErrNoDisplay and manual instructions.
	AutoClearance bool
	// ClearanceWait is how long a capture waits inline for the check to
	// clear. Origins commonly clear themselves within seconds, which keeps
	// playback going without anyone touching the window.
	ClearanceWait time.Duration
	// ClearanceTimeout is how long the window stays open waiting for a human
	// before it is closed again.
	ClearanceTimeout time.Duration
	// Logger receives lifecycle messages.
	Logger *log.Logger
}

const (
	defaultTimeout          = 45 * time.Second
	defaultIdleTimeout      = 2 * time.Minute
	defaultMaxSessions      = 2
	defaultSettle           = 1500 * time.Millisecond
	defaultClearanceWait    = 20 * time.Second
	defaultClearanceTimeout = 5 * time.Minute
)

// engine abstracts the browser process so the resolver's lifecycle can be
// exercised without Chromium.
type engine interface {
	capture(context.Context, Request) ([]Candidate, error)
	close()
}

// Resolver owns a lazily started Chromium process and hands out bounded,
// cancellable capture sessions on it.
type Resolver struct {
	opts         Options
	sem          chan struct{}
	newEngine    func(context.Context, Options) (engine, error)
	newClearance func(context.Context, Options, string) (clearance, error)

	// Serialize process transitions so a new Chromium never races a shutdown
	// for ownership of the persistent profile.
	process chan struct{}

	mu          sync.Mutex
	clearing    *clearanceSession
	eng         engine
	active      int
	idle        *time.Timer
	closed      bool
	starting    chan struct{}
	startCancel context.CancelFunc
	done        chan struct{}
}

// New returns a resolver. Chromium is not started until the first capture, so
// constructing a resolver on a machine without a browser is harmless.
func New(opts Options) *Resolver {
	if opts.Timeout <= 0 {
		opts.Timeout = defaultTimeout
	}
	if opts.IdleTimeout <= 0 {
		opts.IdleTimeout = defaultIdleTimeout
	}
	if opts.MaxSessions <= 0 {
		opts.MaxSessions = defaultMaxSessions
	}
	if opts.ClearanceWait <= 0 {
		opts.ClearanceWait = defaultClearanceWait
	}
	if opts.ClearanceTimeout <= 0 {
		opts.ClearanceTimeout = defaultClearanceTimeout
	}
	if opts.Logger == nil {
		opts.Logger = log.New(io.Discard, "", 0)
	}
	return &Resolver{
		opts:         opts,
		sem:          make(chan struct{}, opts.MaxSessions),
		process:      make(chan struct{}, 1),
		newEngine:    newChromeEngine,
		newClearance: newChromeClearance,
		done:         make(chan struct{}),
	}
}

// Capture opens the page and returns the requests Accept matched, in the order
// they were observed. It returns ErrNoMedia when the page loaded but nothing
// matched, and ErrChallenged when the origin served a bot check instead.
func (r *Resolver) Capture(ctx context.Context, req Request) ([]Candidate, error) {
	if req.PageURL == "" {
		return nil, errors.New("browser capture: page URL is required")
	}
	if req.Accept == nil {
		return nil, errors.New("browser capture: accept predicate is required")
	}
	if req.Settle <= 0 {
		req.Settle = defaultSettle
	}

	ctx, cancel := context.WithTimeout(ctx, r.opts.Timeout)
	defer cancel()

	found, err := r.captureOnce(ctx, req)
	if !errors.Is(err, ErrChallenged) {
		return found, err
	}
	// The clearance cannot be earned headlessly, so put a window on the check.
	// Origins commonly clear themselves within seconds, which turns this into
	// a short pause instead of a failed playback.
	if clearErr := r.Clear(ctx, req.PageURL); clearErr != nil {
		return nil, clearErr
	}
	r.opts.Logger.Printf("bot check cleared, resolving again")
	found, err = r.captureOnce(ctx, req)
	if errors.Is(err, ErrChallenged) {
		return nil, r.challengeError(req.PageURL)
	}
	return found, err
}

func (r *Resolver) captureOnce(ctx context.Context, req Request) ([]Candidate, error) {
	// A visible bot-check window owns the profile directory, which Chromium
	// refuses to share, so there is nothing to capture with until it closes.
	if r.clearancePending() {
		return nil, ErrClearancePending
	}

	select {
	case r.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-r.done:
		return nil, ErrClosed
	}
	defer func() { <-r.sem }()

	eng, err := r.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer r.release()

	found, err := eng.capture(ctx, req)
	if err != nil && isEngineFailure(err) && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		// The browser itself is unusable; drop it so the next capture starts
		// a fresh process instead of reusing a dead connection.
		r.opts.Logger.Printf("browser session failed, restarting Chromium: %v", err)
		r.discard(eng)
	}
	return found, err
}

// challengeError explains how to clear the origin's bot check. The clearance
// is a cookie in the persistent profile and cannot be earned headlessly, so
// the message has to name the profile and the exact command that earns it;
// otherwise the failure reaches the launcher with no way to act on it.
func (r *Resolver) challengeError(pageURL string) error {
	profile := r.opts.ProfileDir
	if profile == "" {
		if dir, err := DefaultProfileDir(); err == nil {
			profile = dir
		}
	}
	if profile == "" {
		return ErrChallenged
	}
	bin := r.opts.Binary
	if bin == "" {
		bin = "chromium"
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
	return fmt.Errorf("%w: stop the anime plugin to release its browser profile, pass the check with %s --user-data-dir=%s --user-agent=%s %s, then close Chromium, restart the plugin and retry",
		ErrChallenged, quote(bin), quote(profile), quote(UserAgent), quote(pageURL))
}

// Close shuts the browser down. It is safe to call more than once.
func (r *Resolver) Close() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	close(r.done)
	if r.startCancel != nil {
		r.startCancel()
	}
	if r.idle != nil {
		r.idle.Stop()
		r.idle = nil
	}
	session := r.clearing
	if session != nil {
		session.cancel()
		r.mu.Unlock()
		<-session.done
		r.mu.Lock()
	}
	eng := r.eng
	r.eng = nil
	r.mu.Unlock()

	if eng != nil {
		r.process <- struct{}{}
		eng.close()
		<-r.process
	}
}

func (r *Resolver) acquire(ctx context.Context) (engine, error) {
	for {
		r.mu.Lock()
		if r.closed {
			r.mu.Unlock()
			return nil, ErrClosed
		}
		if r.clearing != nil {
			r.mu.Unlock()
			return nil, ErrClearancePending
		}
		if err := ctx.Err(); err != nil {
			r.mu.Unlock()
			return nil, err
		}
		if r.eng != nil {
			if r.idle != nil {
				r.idle.Stop()
				r.idle = nil
			}
			r.active++
			eng := r.eng
			r.mu.Unlock()
			return eng, nil
		}
		starting := r.starting
		var result chan error
		if starting == nil {
			starting = make(chan struct{})
			r.starting = starting
			startCtx, cancel := context.WithCancel(ctx)
			r.startCancel = cancel
			result = make(chan error, 1)
			go func() {
				r.process <- struct{}{}
				defer func() { <-r.process }()
				eng, err := r.newEngine(startCtx, r.opts)
				r.mu.Lock()
				if err == nil {
					err = startCtx.Err()
				}
				if r.closed {
					err = ErrClosed
				} else if r.clearing != nil {
					err = ErrClearancePending
				}
				if err == nil {
					r.eng = eng
					r.idle = time.AfterFunc(r.opts.IdleTimeout, r.shutdownIdle)
				}
				r.mu.Unlock()
				if err != nil && eng != nil {
					eng.close()
				}
				cancel()
				r.mu.Lock()
				r.starting, r.startCancel = nil, nil
				result <- err
				close(starting)
				r.mu.Unlock()
			}()
		}
		r.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-r.done:
			return nil, ErrClosed
		case <-starting:
			if result != nil {
				if err := <-result; err != nil {
					return nil, err
				}
			}
		}
	}
}

func (r *Resolver) release() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.active--
	if r.active > 0 || r.closed || r.eng == nil {
		return
	}
	r.idle = time.AfterFunc(r.opts.IdleTimeout, r.shutdownIdle)
}

func (r *Resolver) shutdownIdle() {
	r.process <- struct{}{}
	defer func() { <-r.process }()
	r.mu.Lock()
	if r.active > 0 || r.closed || r.eng == nil {
		r.mu.Unlock()
		return
	}
	eng := r.eng
	r.eng, r.idle = nil, nil
	r.mu.Unlock()

	r.opts.Logger.Printf("browser idle, stopping Chromium")
	eng.close()
}

// discard drops eng only if it is still the current engine, so a slow failing
// session cannot tear down a browser a later session already replaced.
func (r *Resolver) discard(eng engine) {
	r.process <- struct{}{}
	defer func() { <-r.process }()
	r.mu.Lock()
	if r.eng != eng {
		r.mu.Unlock()
		return
	}
	r.eng = nil
	r.mu.Unlock()
	eng.close()
}

// engineFailure marks errors that mean the browser process is unusable, as
// opposed to a page that simply produced nothing.
type engineFailure struct{ err error }

func (e engineFailure) Error() string { return e.err.Error() }
func (e engineFailure) Unwrap() error { return e.err }

func isEngineFailure(err error) bool {
	var failure engineFailure
	return errors.As(err, &failure)
}

// isProfileLocked recognises Chromium's refusal to open a user data directory
// that another instance already owns. It reports the condition through the
// launch output because the launcher surfaces it only as text.
func isProfileLocked(err error) bool {
	message := err.Error()
	return strings.Contains(message, "ProcessSingleton") || strings.Contains(message, "SingletonLock")
}

// FindBinary locates a Chromium-family browser on PATH.
func FindBinary() (string, error) {
	for _, name := range []string{"chromium", "chromium-browser", "google-chrome", "google-chrome-stable", "chrome"} {
		if path, err := exec.LookPath(name); err == nil {
			return path, nil
		}
	}
	return "", ErrUnavailable
}

// DefaultProfileDir is the persistent profile the resolver uses when the
// configuration does not name one. It is kept out of the user's own browser
// data so site logins and cookies are never shared with it.
func DefaultProfileDir() (string, error) {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "tarragon", "anime", "browser"), nil
}

type chromeEngine struct {
	launcher *launcher.Launcher
	browser  *rod.Browser
	cancel   context.CancelFunc
}

func newChromeEngine(ctx context.Context, opts Options) (engine, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	bin := opts.Binary
	if bin == "" {
		found, err := FindBinary()
		if err != nil {
			return nil, err
		}
		bin = found
	}
	profile := opts.ProfileDir
	if profile == "" {
		dir, err := DefaultProfileDir()
		if err != nil {
			return nil, err
		}
		profile = dir
	}
	if err := os.MkdirAll(profile, 0o700); err != nil {
		return nil, fmt.Errorf("prepare browser profile: %w", err)
	}

	l := launcher.New().
		Context(ctx).
		Bin(bin).
		UserDataDir(profile).
		Set("no-first-run").
		Set("no-default-browser-check").
		Set("disable-background-networking").
		Set("disable-sync").
		Set("mute-audio").
		Set("autoplay-policy", "no-user-gesture-required").
		Set("window-size", windowSize).
		Set("user-agent", UserAgent).
		// Keeping player iframes in the host process means a single Network
		// session observes the whole player stack, with no race against
		// attaching to out-of-process frame targets.
		Set("disable-features", "IsolateOrigins,site-per-process,Translate").
		Set("disable-site-isolation-trials").
		// Origins behind a bot check refuse browsers that advertise
		// automation, which would make every capture fail the challenge.
		Delete("enable-automation").
		Set("disable-blink-features", "AutomationControlled")
	if opts.Headless {
		l = l.HeadlessNew(true)
	} else {
		l = l.Headless(false)
	}

	controlURL, err := l.Launch()
	if err != nil {
		if isProfileLocked(err) {
			return nil, fmt.Errorf("%w: %s", ErrProfileLocked, profile)
		}
		return nil, fmt.Errorf("launch Chromium: %w", err)
	}

	// Startup cancellation must not become the shared connection's lifetime.
	lifetime, cancel := context.WithCancel(context.Background())
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	b := rod.New().Context(lifetime).ControlURL(controlURL).NoDefaultDevice()
	if err := b.Connect(); err != nil {
		cancel()
		l.Kill()
		return nil, fmt.Errorf("connect to Chromium: %w", err)
	}
	if !stop() || ctx.Err() != nil {
		cancel()
		l.Kill()
		return nil, ctx.Err()
	}
	opts.Logger.Printf("browser started bin=%s profile=%s headless=%t", bin, profile, opts.Headless)
	return &chromeEngine{launcher: l, browser: b, cancel: cancel}, nil
}

// Wait for clean exit before releasing profile ownership so Chromium can flush
// its cookie store. Kill is a backstop, not the normal shutdown path.
func (e *chromeEngine) close() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	defer e.cancel()
	_ = e.browser.Context(ctx).Close()
	pid := e.launcher.PID()
	for pid > 0 && ctx.Err() == nil {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	// launcher.Cleanup would delete the persistent profile.
	e.launcher.Kill()
	// Kill sends a signal but does not wait for Chromium to release its lock.
	deadline := time.Now().Add(2 * time.Second)
	for pid > 0 && time.Now().Before(deadline) {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (e *chromeEngine) capture(ctx context.Context, req Request) ([]Candidate, error) {
	target, err := (proto.TargetCreateTarget{URL: "about:blank"}).Call(e.browser.Context(ctx))
	if err != nil {
		return nil, engineFailure{fmt.Errorf("open browser page: %w", err)}
	}
	defer func() {
		// Page.Close uses the cancelled page context and may wait for unload handlers.
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _ = (proto.TargetCloseTarget{TargetID: target.TargetID}).Call(e.browser.Context(cleanup))
	}()
	page, err := e.browser.Context(ctx).PageFromTarget(target.TargetID)
	if err != nil {
		return nil, engineFailure{fmt.Errorf("attach browser page: %w", err)}
	}

	if err := (proto.NetworkEnable{}).Call(page); err != nil {
		return nil, engineFailure{fmt.Errorf("observe browser network: %w", err)}
	}

	col := newCollector(req)
	// Observers are attached to a blank page before navigating so requests
	// made during the very first document load cannot be missed.
	stop := page.EachEvent(
		func(ev *proto.NetworkRequestWillBeSent) { col.request(ev) },
		func(ev *proto.NetworkResponseReceived) { col.response(ev) },
		func(ev *proto.NetworkRequestWillBeSentExtraInfo) { col.extraInfo(ev) },
		func(ev *proto.NetworkLoadingFailed) { col.failed(ev) },
	)
	go stop()

	if err := page.Navigate(req.PageURL); err != nil {
		return nil, fmt.Errorf("open %s: %w", req.PageURL, err)
	}

	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if col.challenged() {
				return nil, ErrChallenged
			}
			if found, done := col.settled(); done {
				if len(found) == 0 {
					return nil, ErrNoMedia
				}
				return found, nil
			}
		}
	}
}

// collector turns the raw CDP event stream into accepted candidates. Every
// method is safe to call from the event loop and the waiting goroutine.
type collector struct {
	req   Request
	start time.Time

	mu         sync.Mutex
	pending    map[string]*requestChain
	accepted   []*Candidate
	firstMatch time.Time
	docStatus  int
}

type requestHop struct {
	Candidate
	responseKnown bool
	wantsExtra    bool
	hasExtra      bool
	discarded     bool
}

type requestChain struct {
	hops  []*requestHop
	extra []proto.NetworkHeaders
}

func newCollector(req Request) *collector {
	return &collector{req: req, start: time.Now(), pending: map[string]*requestChain{}}
}

func (c *collector) request(ev *proto.NetworkRequestWillBeSent) {
	candidate := &Candidate{
		URL:      ev.Request.URL,
		Kind:     string(ev.Type),
		Headers:  headerMap(ev.Request.Headers),
		Frame:    string(ev.FrameID),
		Observed: time.Since(c.start),
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	id := string(ev.RequestID)
	chain := c.pending[id]
	if chain == nil {
		chain = &requestChain{}
		c.pending[id] = chain
	}
	if len(chain.hops) > 0 {
		previous := chain.hops[len(chain.hops)-1]
		previous.discarded = true
		previous.responseKnown = true
		previous.wantsExtra = ev.RedirectHasExtraInfo
		c.evaluateLocked(previous)
	}
	chain.hops = append(chain.hops, &requestHop{Candidate: *candidate})
	c.mergeExtraLocked(chain)
}

func (c *collector) response(ev *proto.NetworkResponseReceived) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if chain := c.pending[string(ev.RequestID)]; chain != nil && len(chain.hops) > 0 {
		candidate := chain.hops[len(chain.hops)-1]
		candidate.MIME = ev.Response.MIMEType
		candidate.Status = ev.Response.Status
		candidate.Kind = string(ev.Type)
		candidate.responseKnown = true
		candidate.wantsExtra = ev.HasExtraInfo
		c.mergeExtraLocked(chain)
		c.evaluateLocked(candidate)
	}
	// A bot check replaces the page itself, so only the top document's status
	// is meaningful; sub-resource failures are normal on ad-heavy pages.
	if ev.Type == proto.NetworkResourceTypeDocument && ev.Response.URL == c.req.PageURL {
		c.docStatus = ev.Response.Status
	}
}

func (c *collector) extraInfo(ev *proto.NetworkRequestWillBeSentExtraInfo) {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := string(ev.RequestID)
	chain := c.pending[id]
	if chain == nil {
		chain = &requestChain{}
		c.pending[id] = chain
	}
	chain.extra = append(chain.extra, ev.Headers)
	c.mergeExtraLocked(chain)
}

func (c *collector) mergeExtraLocked(chain *requestChain) {
	// ExtraInfo is ordered within its own stream, not against request/response
	// events. Wait for each hop's flag before consuming or skipping its slot.
	for _, hop := range chain.hops {
		if !hop.responseKnown {
			break
		}
		if !hop.wantsExtra || hop.hasExtra {
			continue
		}
		if len(chain.extra) == 0 {
			break
		}
		for key, value := range headerMap(chain.extra[0]) {
			for old := range hop.Headers {
				if strings.EqualFold(old, key) {
					delete(hop.Headers, old)
				}
			}
			hop.Headers[key] = value
		}
		chain.extra = chain.extra[1:]
		hop.hasExtra = true
		c.evaluateLocked(hop)
	}
}

func (c *collector) failed(ev *proto.NetworkLoadingFailed) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if chain := c.pending[string(ev.RequestID)]; chain != nil && len(chain.hops) > 0 {
		hop := chain.hops[len(chain.hops)-1]
		hop.discarded = true
		c.evaluateLocked(hop)
	}
}

func (c *collector) evaluateLocked(hop *requestHop) {
	wanted := !hop.discarded && hop.Status >= 200 && hop.Status < 300 &&
		(!hop.wantsExtra || hop.hasExtra) && c.req.Accept(hop.Candidate)
	for i, candidate := range c.accepted {
		if candidate == &hop.Candidate {
			if wanted {
				wanted = false // Already retained; still re-evaluate readiness.
			} else {
				c.accepted = append(c.accepted[:i], c.accepted[i+1:]...)
			}
			break
		}
	}
	if wanted {
		c.accepted = append(c.accepted, &hop.Candidate)
	}
	for _, candidate := range c.accepted {
		if c.req.Ready == nil || c.req.Ready(*candidate) {
			if c.firstMatch.IsZero() {
				c.firstMatch = time.Now()
			}
			return
		}
	}
	c.firstMatch = time.Time{}
}

func (c *collector) challenged() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.docStatus == 403 && len(c.accepted) == 0
}

// settled reports the accepted candidates once the settle window has passed
// since the first ready match, so late variants and subtitles are still collected.
func (c *collector) settled() ([]Candidate, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.firstMatch.IsZero() {
		return nil, false
	}
	if time.Since(c.firstMatch) < c.req.Settle {
		return nil, false
	}
	return c.snapshotLocked(), true
}

func (c *collector) matches() []Candidate {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.snapshotLocked()
}

func (c *collector) snapshotLocked() []Candidate {
	out := make([]Candidate, 0, len(c.accepted))
	for _, candidate := range c.accepted {
		copy := *candidate
		copy.Headers = maps.Clone(candidate.Headers)
		out = append(out, copy)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Observed < out[j].Observed })
	return out
}

func headerMap(headers proto.NetworkHeaders) map[string]string {
	out := make(map[string]string, len(headers))
	for key, value := range headers {
		out[key] = value.String()
	}
	return out
}
