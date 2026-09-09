package browser

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

// fakeClearance stands in for the visible window so the challenge workflow can
// be tested without a display.
type fakeClearance struct {
	mu      sync.Mutex
	ok      bool
	err     error
	closed  int
	closing chan struct{}
	finish  chan struct{}
}

func (c *fakeClearance) cleared(context.Context) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ok, c.err
}

func (c *fakeClearance) close() {
	if c.closing != nil {
		close(c.closing)
		<-c.finish
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed++
}

func (c *fakeClearance) pass() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ok = true
}

func (c *fakeClearance) closes() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

func clearanceOptions() Options {
	return Options{
		AutoClearance: true, ClearanceWait: time.Second, ClearanceTimeout: 5 * time.Second,
		IdleTimeout: time.Hour,
	}
}

// withDisplay makes the display probe succeed, since the tests must not depend
// on the machine running them having one.
func withDisplay(t *testing.T) {
	t.Helper()
	t.Setenv("WAYLAND_DISPLAY", "wayland-test")
}

func TestChallengeOpensAWindowAndRetriesOnceCleared(t *testing.T) {
	withDisplay(t)
	var captures atomic.Int32
	window := &fakeClearance{}
	eng := &fakeEngine{err: ErrChallenged}
	resolver := newTestResolver(t, clearanceOptions(), func(context.Context, Options) (engine, error) {
		return eng, nil
	})
	resolver.newClearance = func(context.Context, Options, string) (clearance, error) {
		window.pass() // The origin clears itself, as most do.
		return window, nil
	}
	eng.hook = func() error {
		if captures.Add(1) == 1 {
			return ErrChallenged
		}
		return nil
	}

	_, err := resolver.Capture(t.Context(), testRequest())
	if err != nil {
		t.Fatalf("Capture() error = %v, want the retry after clearance to succeed", err)
	}
	if got := captures.Load(); got != 2 {
		t.Fatalf("captures = %d, want the challenged capture and one retry", got)
	}
	if window.closes() == 0 {
		t.Fatal("clearance window was left open after the check passed")
	}
}

func TestChallengeReportsPendingWhileAHumanIsNeeded(t *testing.T) {
	withDisplay(t)
	window := &fakeClearance{}
	eng := &fakeEngine{err: ErrChallenged}
	resolver := newTestResolver(t, clearanceOptions(), func(context.Context, Options) (engine, error) {
		return eng, nil
	})
	resolver.newClearance = func(context.Context, Options, string) (clearance, error) { return window, nil }

	_, err := resolver.Capture(t.Context(), testRequest())
	if !errors.Is(err, ErrClearancePending) {
		t.Fatalf("Capture() = %v, want ErrClearancePending", err)
	}
	// The window stays open for the human, so further captures cannot run.
	if !resolver.clearancePending() {
		t.Fatal("clearance window was closed while still unsolved")
	}
	if _, err := resolver.Capture(t.Context(), testRequest()); !errors.Is(err, ErrClearancePending) {
		t.Fatalf("second Capture() = %v, want ErrClearancePending", err)
	}

	window.pass()
	deadline := time.Now().Add(3 * time.Second)
	for resolver.clearancePending() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if resolver.clearancePending() {
		t.Fatal("clearance window was not closed after the check passed")
	}
}

func TestClearanceOpensOneWindowForConcurrentChallenges(t *testing.T) {
	withDisplay(t)
	var windows atomic.Int32
	window := &fakeClearance{}
	resolver := newTestResolver(t, clearanceOptions(), func(context.Context, Options) (engine, error) {
		return &fakeEngine{err: ErrChallenged}, nil
	})
	resolver.newClearance = func(context.Context, Options, string) (clearance, error) {
		windows.Add(1)
		return window, nil
	}

	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = resolver.Capture(t.Context(), testRequest())
		}()
	}
	wg.Wait()
	if got := windows.Load(); got != 1 {
		t.Fatalf("clearance windows = %d, want a single shared window", got)
	}
}

func TestClearanceClosesTheWindowWhenNobodySolvesIt(t *testing.T) {
	withDisplay(t)
	window := &fakeClearance{}
	opts := clearanceOptions()
	opts.ClearanceTimeout = 1200 * time.Millisecond
	resolver := newTestResolver(t, opts, func(context.Context, Options) (engine, error) {
		return &fakeEngine{err: ErrChallenged}, nil
	})
	resolver.newClearance = func(context.Context, Options, string) (clearance, error) { return window, nil }

	if _, err := resolver.Capture(t.Context(), testRequest()); !errors.Is(err, ErrClearancePending) {
		t.Fatalf("Capture() = %v, want ErrClearancePending", err)
	}
	deadline := time.Now().Add(4 * time.Second)
	for window.closes() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if window.closes() == 0 {
		t.Fatal("unattended clearance window kept the profile locked")
	}
}

func TestClearanceIsSkippedWithoutAutoClearance(t *testing.T) {
	withDisplay(t)
	opts := clearanceOptions()
	opts.AutoClearance = false
	opts.ProfileDir = "/state/anime/browser"
	resolver := newTestResolver(t, opts, func(context.Context, Options) (engine, error) {
		return &fakeEngine{err: ErrChallenged}, nil
	})
	resolver.newClearance = func(context.Context, Options, string) (clearance, error) {
		t.Fatal("opened a window while auto clearance was disabled")
		return nil, nil
	}

	_, err := resolver.Capture(t.Context(), testRequest())
	if !errors.Is(err, ErrChallenged) || errors.Is(err, ErrClearancePending) {
		t.Fatalf("Capture() = %v, want the manual challenge instructions", err)
	}
}

func TestClearanceRequiresADisplay(t *testing.T) {
	t.Setenv("WAYLAND_DISPLAY", "")
	t.Setenv("DISPLAY", "")
	resolver := newTestResolver(t, clearanceOptions(), func(context.Context, Options) (engine, error) {
		return &fakeEngine{err: ErrChallenged}, nil
	})
	resolver.newClearance = func(context.Context, Options, string) (clearance, error) {
		t.Fatal("opened a window on a machine with no display")
		return nil, nil
	}

	_, err := resolver.Capture(t.Context(), testRequest())
	if !errors.Is(err, ErrNoDisplay) {
		t.Fatalf("Capture() = %v, want ErrNoDisplay", err)
	}
	if !errors.Is(err, ErrChallenged) || !strings.Contains(err.Error(), "--user-data-dir=") {
		t.Fatalf("no-display error lost manual fallback: %v", err)
	}
}

func TestIsClearanceCookie(t *testing.T) {
	for _, name := range []string{"cf_clearance", "CF_Clearance"} {
		if !isClearanceCookie(name) {
			t.Fatalf("isClearanceCookie(%q) = false", name)
		}
	}
	for _, name := range []string{"session", "_ga", "__cfruid", "__ddg1_", "__ddg2_", "not_clearance"} {
		if isClearanceCookie(name) {
			t.Fatalf("isClearanceCookie(%q) = true", name)
		}
	}
}

func TestClearanceWaitsForProfileReleaseBeforeRetry(t *testing.T) {
	withDisplay(t)
	window := &fakeClearance{ok: true, closing: make(chan struct{}), finish: make(chan struct{})}
	var starts atomic.Int32
	resolver := newTestResolver(t, clearanceOptions(), func(context.Context, Options) (engine, error) {
		if starts.Add(1) == 1 {
			return &fakeEngine{err: ErrChallenged}, nil
		}
		if window.closes() != 1 {
			t.Error("capture restarted before the clearance browser released the profile")
		}
		return &fakeEngine{}, nil
	})
	resolver.newClearance = func(context.Context, Options, string) (clearance, error) { return window, nil }
	t.Cleanup(func() { close(window.finish) })
	result := make(chan error, 1)
	go func() {
		_, err := resolver.Capture(t.Context(), testRequest())
		result <- err
	}()
	<-window.closing
	if !resolver.clearancePending() {
		t.Fatal("profile declared free before browser shutdown completed")
	}
	if _, err := resolver.Capture(t.Context(), testRequest()); !errors.Is(err, ErrClearancePending) {
		t.Fatalf("concurrent Capture() = %v, want pending", err)
	}
	window.finish <- struct{}{}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if starts.Load() != 2 {
		t.Fatalf("starts = %d, want initial capture and retry", starts.Load())
	}
}

func TestClearanceRetriesOnlyOnce(t *testing.T) {
	withDisplay(t)
	var windows atomic.Int32
	eng := &fakeEngine{err: ErrChallenged}
	resolver := newTestResolver(t, clearanceOptions(), func(context.Context, Options) (engine, error) { return eng, nil })
	resolver.newClearance = func(context.Context, Options, string) (clearance, error) {
		windows.Add(1)
		return &fakeClearance{ok: true}, nil
	}
	_, err := resolver.Capture(t.Context(), testRequest())
	if !errors.Is(err, ErrChallenged) || !strings.Contains(err.Error(), "--user-data-dir=") {
		t.Fatalf("retry error = %v, want actionable challenge instructions", err)
	}
	if captures, _ := eng.counts(); captures != 2 || windows.Load() != 1 {
		t.Fatalf("captures=%d windows=%d, want 2 and 1", captures, windows.Load())
	}
}

func TestClearanceFailureReleasesProfile(t *testing.T) {
	withDisplay(t)
	for _, launchFailure := range []bool{true, false} {
		t.Run(map[bool]string{true: "launch", false: "window"}[launchFailure], func(t *testing.T) {
			cause := errors.New("window unavailable")
			window := &fakeClearance{err: cause}
			resolver := newTestResolver(t, clearanceOptions(), func(context.Context, Options) (engine, error) {
				return &fakeEngine{}, nil
			})
			resolver.newClearance = func(context.Context, Options, string) (clearance, error) {
				if launchFailure {
					return nil, cause
				}
				return window, nil
			}
			if err := resolver.Clear(t.Context(), testRequest().PageURL); !errors.Is(err, cause) || !errors.Is(err, ErrChallenged) {
				t.Fatalf("Clear() = %v, want cause and manual fallback", err)
			}
			if resolver.clearancePending() {
				t.Fatal("failed session still owns the profile")
			}
			if !launchFailure && window.closes() != 1 {
				t.Fatal("failed window was not closed")
			}
			if _, err := resolver.Capture(t.Context(), testRequest()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestClearanceCancellationAndCloseDuringLaunch(t *testing.T) {
	withDisplay(t)
	started := make(chan struct{})
	window := &fakeClearance{}
	resolver := newTestResolver(t, clearanceOptions(), func(context.Context, Options) (engine, error) {
		return &fakeEngine{}, nil
	})
	resolver.newClearance = func(ctx context.Context, _ Options, _ string) (clearance, error) {
		close(started)
		<-ctx.Done()
		return window, ctx.Err()
	}
	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() { result <- resolver.Clear(ctx, testRequest().PageURL) }()
	<-started
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("Clear() = %v, want cancellation", err)
	}
	resolver.Close()
	if window.closes() != 1 || resolver.clearancePending() {
		t.Fatal("Close returned before disposing the late clearance window")
	}
	if err := resolver.Clear(t.Context(), testRequest().PageURL); !errors.Is(err, ErrClosed) {
		t.Fatalf("Clear after Close = %v", err)
	}
}

func TestClearanceDoesNotInterruptActiveCaptures(t *testing.T) {
	withDisplay(t)
	active := make(chan struct{})
	finish := make(chan struct{})
	eng := &fakeEngine{block: finish, hook: func() error { close(active); return nil }}
	resolver := newTestResolver(t, clearanceOptions(), func(context.Context, Options) (engine, error) { return eng, nil })
	resolver.newClearance = func(context.Context, Options, string) (clearance, error) {
		if _, closed := eng.counts(); closed != 1 {
			t.Error("capture browser not closed before clearance launch")
		}
		return &fakeClearance{ok: true}, nil
	}
	capture := make(chan error, 1)
	go func() { _, err := resolver.Capture(t.Context(), testRequest()); capture <- err }()
	<-active
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := resolver.Clear(ctx, testRequest().PageURL); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Clear while busy = %v", err)
	}
	if _, closed := eng.counts(); closed != 0 {
		t.Fatal("clearance interrupted an active capture")
	}
	close(finish)
	if err := <-capture; err != nil {
		t.Fatal(err)
	}
	if err := resolver.Clear(t.Context(), testRequest().PageURL); err != nil {
		t.Fatal(err)
	}
}

func TestCloseDuringClearanceDoesNotWaitForStalledCaptureStartup(t *testing.T) {
	withDisplay(t)
	started, finish := make(chan struct{}), make(chan struct{})
	resolver := newTestResolver(t, clearanceOptions(), func(context.Context, Options) (engine, error) {
		close(started)
		<-finish
		return &fakeEngine{}, nil
	})
	t.Cleanup(func() { close(finish) })
	go func() { _, _ = resolver.Capture(t.Context(), testRequest()) }()
	<-started
	resolver.mu.Lock()
	startup := resolver.starting
	resolver.mu.Unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := resolver.Clear(ctx, testRequest().PageURL); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Clear() = %v", err)
	}
	closed := make(chan struct{})
	go func() { resolver.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close blocked on capture startup through the clearance session")
	}
	finish <- struct{}{}
	<-startup
}

func TestChromeClearanceRequiresCookieAndSuccessfulDocument(t *testing.T) {
	for _, tt := range []struct {
		name, cookies string
		loaded, want  bool
	}{
		{"cleared", `{"cookies":[{"name":"cf_clearance"}]}`, true, true},
		{"stale clearance", `{"cookies":[{"name":"cf_clearance"}]}`, false, false},
		{"tracking cookie", `{"cookies":[{"name":"__ddg1_"}]}`, true, false},
		{"no cookie", `{"cookies":[]}`, true, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := lifecycleCDP{call: func(_ context.Context, method string, params interface{}) ([]byte, error) {
				switch method {
				case "Target.createTarget":
					return []byte(`{"targetId":"tab"}`), nil
				case "Target.attachToTarget":
					return []byte(`{"sessionId":"session"}`), nil
				case "Network.getCookies":
					if urls := params.(proto.NetworkGetCookies).Urls; len(urls) != 1 || urls[0] != "https://example.test/redirected" {
						t.Errorf("cookie URLs = %v, want final document URL", urls)
					}
					return []byte(tt.cookies), nil
				}
				return []byte(`{}`), nil
			}}
			b := rod.New().Context(t.Context()).Client(client).NoDefaultDevice()
			if err := b.Connect(); err != nil {
				t.Fatal(err)
			}
			page, err := b.Page(proto.TargetCreateTarget{})
			if err != nil {
				t.Fatal(err)
			}
			c := &chromeClearance{page: page, pageURL: "https://example.test/redirected", loaded: tt.loaded}
			if got, err := c.cleared(t.Context()); err != nil || got != tt.want {
				t.Fatalf("cleared() = %v, %v; want %v", got, err, tt.want)
			}
		})
	}
}
