package browser

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

var (
	// ErrClearancePending means a window still owns the browser profile.
	ErrClearancePending = errors.New("complete the bot check in the open browser window, wait for it to close, then retry playback")
	ErrNoDisplay        = errors.New("no display available for a visible browser window")
)

type clearance interface {
	cleared(context.Context) (bool, error)
	close()
}

type clearanceSession struct {
	done   chan struct{}
	cancel context.CancelFunc
	err    error // Published by closing done, after the profile is released.
}

func hasDisplay() bool {
	return os.Getenv("WAYLAND_DISPLAY") != "" || os.Getenv("DISPLAY") != ""
}

// Clear opens a visible browser at pageURL and waits a bounded time for the
// check. If it needs longer, the window stays open and callers retry later.
// Cancellation stops waiting; Close or ClearanceTimeout closes the window.
func (r *Resolver) Clear(ctx context.Context, pageURL string) error {
	if pageURL == "" {
		return errors.New("browser clearance: page URL is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return ErrClosed
	}
	if !r.opts.AutoClearance {
		r.mu.Unlock()
		return r.challengeError(pageURL)
	}
	if !hasDisplay() {
		r.mu.Unlock()
		return fmt.Errorf("%w: %w", ErrNoDisplay, r.challengeError(pageURL))
	}
	session := r.clearing
	if session == nil {
		lifetime, cancel := context.WithTimeout(context.Background(), r.opts.ClearanceTimeout)
		session = &clearanceSession{done: make(chan struct{}), cancel: cancel}
		r.clearing = session
		if r.startCancel != nil {
			r.startCancel()
		}
		go r.watchClearance(lifetime, session, pageURL)
	}
	r.mu.Unlock()

	wait, cancel := context.WithTimeout(ctx, r.opts.ClearanceWait)
	defer cancel()
	select {
	case <-session.done:
		return session.err
	case <-r.done:
		return ErrClosed
	case <-wait.Done():
		if err := ctx.Err(); err != nil {
			return err
		}
		return ErrClearancePending
	}
}

func (r *Resolver) watchClearance(ctx context.Context, session *clearanceSession, pageURL string) {
	var window clearance
	var result error
	locked := false
	defer func() {
		if window != nil {
			window.close()
		}
		if locked {
			<-r.process
		}
		session.cancel()
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.closed {
			result = ErrClosed
		}
		if result != nil {
			r.opts.Logger.Printf("browser bot check: %v", result)
		} else {
			r.opts.Logger.Printf("browser bot check passed; profile released for capture")
		}
		session.err = result
		r.clearing = nil
		close(session.done)
	}()

	// Drain captures already using the process without interrupting them.
	// acquire rejects queued/new work as soon as r.clearing is set.
	slots := 0
	defer func() {
		for range slots {
			<-r.sem
		}
	}()
	for slots < cap(r.sem) {
		select {
		case r.sem <- struct{}{}:
			slots++
		case <-ctx.Done():
			result = fmt.Errorf("timed out waiting for the browser profile; %w", r.challengeError(pageURL))
			return
		}
	}
	// Let callers already queued on sem reach acquire and get the pending
	// error now, rather than waiting until this window times out and opening
	// another one. r.clearing prevents them from starting a capture.
	for slots > 0 {
		<-r.sem
		slots--
	}

	// Startup and idle/crash shutdown may still be releasing the profile even
	// after captures finish. All process transitions use the same lock.
	select {
	case r.process <- struct{}{}:
	case <-ctx.Done():
		result = fmt.Errorf("timed out waiting for browser shutdown; %w", r.challengeError(pageURL))
		return
	}
	locked = true
	if err := ctx.Err(); err != nil {
		result = err
		return
	}
	r.stopEngine()
	window, result = r.newClearance(ctx, r.opts, pageURL)
	if result != nil {
		result = fmt.Errorf("open browser for the bot check: %w; %w", result, r.challengeError(pageURL))
		return
	}
	r.opts.Logger.Printf("browser opened for the origin's bot check: %s", pageURL)

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		check, cancel := context.WithTimeout(ctx, 5*time.Second)
		cleared, err := window.cleared(check)
		cancel()
		if err != nil {
			result = fmt.Errorf("browser bot check window failed: %w; %w", err, r.challengeError(pageURL))
			return
		}
		if cleared {
			return
		}
		select {
		case <-ctx.Done():
			result = fmt.Errorf("bot check not passed within %s; %w", r.opts.ClearanceTimeout, r.challengeError(pageURL))
			return
		case <-ticker.C:
		}
	}
}

func (r *Resolver) clearancePending() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.clearing != nil
}

// Caller owns the process transition gate and has drained active captures.
func (r *Resolver) stopEngine() {
	r.mu.Lock()
	eng := r.eng
	r.eng = nil
	if r.idle != nil {
		r.idle.Stop()
		r.idle = nil
	}
	r.mu.Unlock()
	if eng != nil {
		eng.close()
	}
}

func isClearanceCookie(name string) bool {
	name = strings.ToLower(name)
	return name == "cf_clearance"
}

type chromeClearance struct {
	engine  *chromeEngine
	page    *rod.Page
	pageURL string
	mu      sync.Mutex
	loaded  bool
	cancel  context.CancelFunc
}

func newChromeClearance(ctx context.Context, opts Options, pageURL string) (clearance, error) {
	opts.Headless = false
	eng, err := newChromeEngine(ctx, opts)
	if err != nil {
		return nil, err
	}
	chrome := eng.(*chromeEngine)
	page, err := chrome.browser.Context(ctx).Page(proto.TargetCreateTarget{URL: "about:blank"})
	if err != nil {
		eng.close()
		return nil, fmt.Errorf("open the challenged page: %w", err)
	}
	observe, cancel := context.WithCancel(ctx)
	page = page.Context(observe)
	c := &chromeClearance{engine: chrome, page: page, pageURL: pageURL, cancel: cancel}
	go page.EachEvent(
		func(ev *proto.NetworkRequestWillBeSent) {
			if ev.Type == proto.NetworkResourceTypeDocument && ev.FrameID == page.FrameID {
				c.mu.Lock()
				c.loaded = false
				c.pageURL = ev.Request.URL
				c.mu.Unlock()
			}
		},
		func(ev *proto.NetworkResponseReceived) {
			if ev.Type == proto.NetworkResourceTypeDocument && ev.FrameID == page.FrameID {
				c.mu.Lock()
				c.loaded = ev.Response.Status >= 200 && ev.Response.Status < 300
				for name, value := range ev.Response.Headers {
					if strings.EqualFold(name, "cf-mitigated") && value.Str() == "challenge" {
						c.loaded = false
					}
				}
				c.pageURL = ev.Response.URL
				c.mu.Unlock()
			}
		},
	)()
	if err := page.Navigate(pageURL); err != nil {
		c.close()
		return nil, fmt.Errorf("navigate to the challenged page: %w", err)
	}
	return c, nil
}

func (c *chromeClearance) cleared(ctx context.Context) (bool, error) {
	c.mu.Lock()
	pageURL, loaded := c.pageURL, c.loaded
	c.mu.Unlock()
	cookies, err := (proto.NetworkGetCookies{Urls: []string{pageURL}}).Call(c.page.Context(ctx))
	if err != nil {
		return false, err
	}
	// A stale clearance alone is not proof that the check passed: the watch
	// document must also load successfully. DDoS-Guard's tracking cookies do
	// not establish clearance, so they deliberately do not qualify.
	for _, cookie := range cookies.Cookies {
		if isClearanceCookie(cookie.Name) && loaded {
			return true, nil
		}
	}
	return false, nil
}

func (c *chromeClearance) close() {
	c.cancel()
	c.engine.close()
}
