package browser

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
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

// Clear opens an ordinary visible browser at pageURL and waits a bounded time
// for it to exit. The user must complete the check and close that window;
// Chromium's normal shutdown then persists the clearance cookie.
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

// processClearance deliberately launches Chromium without a DevTools
// connection. Cloudflare rejects the CDP-controlled launch even when the
// window is visible; a normal browser process accepts the human checkbox.
type processClearance struct {
	cmd  *exec.Cmd
	done chan struct{}
	err  error
}

func newChromeClearance(ctx context.Context, opts Options, pageURL string) (clearance, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	bin := opts.Binary
	if bin == "" {
		var err error
		bin, err = FindBinary()
		if err != nil {
			return nil, err
		}
	}
	profile := opts.ProfileDir
	if profile == "" {
		var err error
		profile, err = DefaultProfileDir()
		if err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(profile, 0o700); err != nil {
		return nil, fmt.Errorf("prepare browser profile: %w", err)
	}
	cmd := exec.Command(bin,
		"--user-data-dir="+profile,
		"--no-first-run",
		"--no-default-browser-check",
		"--new-window",
		pageURL,
	)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		if isProfileLocked(err) {
			return nil, fmt.Errorf("%w: %s", ErrProfileLocked, profile)
		}
		return nil, fmt.Errorf("open Chromium for the bot check: %w", err)
	}
	c := &processClearance{cmd: cmd, done: make(chan struct{})}
	go func() {
		c.err = cmd.Wait()
		close(c.done)
	}()
	return c, nil
}

func (c *processClearance) cleared(ctx context.Context) (bool, error) {
	select {
	case <-c.done:
		if c.err != nil {
			return false, fmt.Errorf("clearance browser exited: %w", c.err)
		}
		return true, nil
	case <-ctx.Done():
		// The watcher uses a short context for polling. An open browser is
		// expected here, not an error; the outer timeout handles cancellation.
		return false, nil
	}
}

func (c *processClearance) close() {
	select {
	case <-c.done:
		return
	default:
	}
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Signal(syscall.SIGTERM)
	}
	select {
	case <-c.done:
	case <-time.After(5 * time.Second):
		_ = c.cmd.Process.Kill()
		<-c.done
	}
}
