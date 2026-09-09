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
// same profile. When the origin retires the clearance, Capture reports
// ErrChallenged and the step has to be repeated.
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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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
	// Settle is how long to keep observing after the first accepted request
	// so alternates, such as higher quality variants or subtitle tracks, are
	// not missed. Zero returns as soon as something matches.
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
	// Logger receives lifecycle messages.
	Logger *log.Logger
}

const (
	defaultTimeout     = 45 * time.Second
	defaultIdleTimeout = 2 * time.Minute
	defaultMaxSessions = 2
	defaultSettle      = 1500 * time.Millisecond
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
	opts      Options
	sem       chan struct{}
	newEngine func(context.Context, Options) (engine, error)

	mu     sync.Mutex
	eng    engine
	active int
	idle   *time.Timer
	closed bool
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
	if opts.Logger == nil {
		opts.Logger = log.New(io.Discard, "", 0)
	}
	return &Resolver{
		opts:      opts,
		sem:       make(chan struct{}, opts.MaxSessions),
		newEngine: newChromeEngine,
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

	select {
	case r.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-r.sem }()

	eng, err := r.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer r.release()

	ctx, cancel := context.WithTimeout(ctx, r.opts.Timeout)
	defer cancel()

	found, err := eng.capture(ctx, req)
	if err != nil && isEngineFailure(err) {
		// The browser itself is unusable; drop it so the next capture starts
		// a fresh process instead of reusing a dead connection.
		r.opts.Logger.Printf("browser session failed, restarting Chromium: %v", err)
		r.discard(eng)
	}
	return found, err
}

// Close shuts the browser down. It is safe to call more than once.
func (r *Resolver) Close() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	if r.idle != nil {
		r.idle.Stop()
		r.idle = nil
	}
	eng := r.eng
	r.eng = nil
	r.mu.Unlock()

	if eng != nil {
		eng.close()
	}
}

func (r *Resolver) acquire(ctx context.Context) (engine, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, ErrClosed
	}
	if r.idle != nil {
		r.idle.Stop()
		r.idle = nil
	}
	if r.eng == nil {
		eng, err := r.newEngine(ctx, r.opts)
		if err != nil {
			return nil, err
		}
		r.eng = eng
	}
	r.active++
	return r.eng, nil
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
}

func newChromeEngine(_ context.Context, opts Options) (engine, error) {
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

	b := rod.New().ControlURL(controlURL).NoDefaultDevice()
	if err := b.Connect(); err != nil {
		l.Kill()
		return nil, fmt.Errorf("connect to Chromium: %w", err)
	}
	opts.Logger.Printf("browser started bin=%s profile=%s headless=%t", bin, profile, opts.Headless)
	return &chromeEngine{launcher: l, browser: b}, nil
}

func (e *chromeEngine) close() {
	_ = e.browser.Close()
	// launcher.Cleanup would delete the user data directory. The profile is
	// persistent and holds the origin's clearance cookie, so the process is
	// killed without touching it.
	e.launcher.Kill()
}

func (e *chromeEngine) capture(ctx context.Context, req Request) ([]Candidate, error) {
	// Binding the page to ctx makes cancellation tear the tab down instead of
	// leaving it loading in the shared browser.
	page, err := e.browser.Context(ctx).Page(proto.TargetCreateTarget{URL: "about:blank"})
	if err != nil {
		return nil, engineFailure{fmt.Errorf("open browser page: %w", err)}
	}
	defer func() { _ = page.Close() }()

	if err := (proto.NetworkEnable{}).Call(page); err != nil {
		return nil, engineFailure{fmt.Errorf("observe browser network: %w", err)}
	}

	col := newCollector(req)
	// Observers are attached to a blank page before navigating so requests
	// made during the very first document load cannot be missed.
	stop := page.EachEvent(
		func(ev *proto.NetworkRequestWillBeSent) { col.request(ev) },
		func(ev *proto.NetworkResponseReceived) { col.response(ev) },
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
			if found := col.matches(); len(found) > 0 {
				return found, nil
			}
			if col.challenged() {
				return nil, ErrChallenged
			}
			return nil, ctx.Err()
		case <-ticker.C:
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
	pending    map[string]*Candidate
	accepted   []*Candidate
	firstMatch time.Time
	docStatus  int
}

func newCollector(req Request) *collector {
	return &collector{req: req, start: time.Now(), pending: map[string]*Candidate{}}
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
	c.pending[string(ev.RequestID)] = candidate
	if c.req.Accept(*candidate) {
		c.accepted = append(c.accepted, candidate)
		if c.firstMatch.IsZero() {
			c.firstMatch = time.Now()
		}
	}
	c.mu.Unlock()
}

func (c *collector) response(ev *proto.NetworkResponseReceived) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if candidate, ok := c.pending[string(ev.RequestID)]; ok {
		candidate.MIME = ev.Response.MIMEType
		candidate.Status = ev.Response.Status
	}
	// A bot check replaces the page itself, so only the top document's status
	// is meaningful; sub-resource failures are normal on ad-heavy pages.
	if ev.Type == proto.NetworkResourceTypeDocument && ev.Response.URL == c.req.PageURL {
		c.docStatus = ev.Response.Status
	}
}

func (c *collector) challenged() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.docStatus == 403 && len(c.accepted) == 0
}

// settled reports the accepted candidates once the settle window has passed
// since the first match, so late higher quality variants are still collected.
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
		out = append(out, *candidate)
	}
	return out
}

func headerMap(headers proto.NetworkHeaders) map[string]string {
	out := make(map[string]string, len(headers))
	for key, value := range headers {
		out[key] = value.String()
	}
	return out
}
