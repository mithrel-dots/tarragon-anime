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
	"github.com/go-rod/rod/lib/cdp"
	"github.com/go-rod/rod/lib/proto"
	"github.com/ysmood/gson"
)

// fakeEngine stands in for Chromium so the resolver's lifecycle can be tested
// without a browser process.
type fakeEngine struct {
	mu       sync.Mutex
	captures int
	closed   int
	block    chan struct{}
	err      error
	result   []Candidate
}

func (e *fakeEngine) capture(ctx context.Context, _ Request) ([]Candidate, error) {
	e.mu.Lock()
	e.captures++
	block := e.block
	err, result := e.err, e.result
	e.mu.Unlock()

	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return result, err
}

func (e *fakeEngine) close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closed++
}

func (e *fakeEngine) counts() (captures, closed int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.captures, e.closed
}

func testRequest() Request {
	return Request{PageURL: "https://example.test/watch", Accept: func(Candidate) bool { return true }}
}

func newTestResolver(t *testing.T, opts Options, factory func(context.Context, Options) (engine, error)) *Resolver {
	t.Helper()
	resolver := New(opts)
	resolver.newEngine = factory
	t.Cleanup(resolver.Close)
	return resolver
}

func TestResolverStartsBrowserLazily(t *testing.T) {
	var started atomic.Int32
	eng := &fakeEngine{result: []Candidate{{URL: "https://cdn.test/a.mp4"}}}
	resolver := newTestResolver(t, Options{IdleTimeout: time.Hour}, func(context.Context, Options) (engine, error) {
		started.Add(1)
		return eng, nil
	})

	if got := started.Load(); got != 0 {
		t.Fatalf("browser started before first capture: started = %d, want 0", got)
	}
	for range 3 {
		if _, err := resolver.Capture(t.Context(), testRequest()); err != nil {
			t.Fatalf("Capture() error = %v", err)
		}
	}
	if got := started.Load(); got != 1 {
		t.Fatalf("browser starts = %d, want 1 reused instance", got)
	}
}

func TestChallengeErrorNamesTheProfileAndCommand(t *testing.T) {
	eng := &fakeEngine{err: ErrChallenged}
	resolver := newTestResolver(t, Options{
		Binary: "/usr/bin/chromium", ProfileDir: "/state/anime/browser", IdleTimeout: time.Hour,
	}, func(context.Context, Options) (engine, error) { return eng, nil })

	_, err := resolver.Capture(t.Context(), testRequest())
	if !errors.Is(err, ErrChallenged) {
		t.Fatalf("Capture() = %v, want ErrChallenged", err)
	}
	for _, want := range []string{"/usr/bin/chromium", "--user-data-dir=/state/anime/browser", testRequest().PageURL} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("challenge error %q does not tell the user to %q", err, want)
		}
	}
}

func TestChallengeErrorFallsBackToTheDefaultProfile(t *testing.T) {
	profile, err := DefaultProfileDir()
	if err != nil {
		t.Skipf("no default profile dir: %v", err)
	}
	eng := &fakeEngine{err: ErrChallenged}
	resolver := newTestResolver(t, Options{IdleTimeout: time.Hour},
		func(context.Context, Options) (engine, error) { return eng, nil })

	_, captureErr := resolver.Capture(t.Context(), testRequest())
	if !strings.Contains(captureErr.Error(), profile) {
		t.Fatalf("challenge error %q does not name the default profile %q", captureErr, profile)
	}
}

func TestResolverShutsDownWhenIdle(t *testing.T) {
	eng := &fakeEngine{}
	resolver := newTestResolver(t, Options{IdleTimeout: 20 * time.Millisecond},
		func(context.Context, Options) (engine, error) { return eng, nil })

	if _, err := resolver.Capture(t.Context(), testRequest()); !errors.Is(err, ErrNoMedia) && err != nil {
		t.Fatalf("Capture() error = %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, closed := eng.counts(); closed == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("browser was not stopped after the idle timeout")
		}
		time.Sleep(5 * time.Millisecond)
	}

	if _, err := resolver.Capture(t.Context(), testRequest()); err != nil {
		t.Fatalf("Capture() after idle shutdown error = %v", err)
	}
	if captures, _ := eng.counts(); captures != 2 {
		t.Fatalf("captures = %d, want 2 after restart", captures)
	}
}

func TestResolverDoesNotShutDownWhileBusy(t *testing.T) {
	eng := &fakeEngine{block: make(chan struct{})}
	resolver := newTestResolver(t, Options{IdleTimeout: 10 * time.Millisecond, MaxSessions: 2},
		func(context.Context, Options) (engine, error) { return eng, nil })

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = resolver.Capture(t.Context(), testRequest())
	}()

	time.Sleep(80 * time.Millisecond)
	if _, closed := eng.counts(); closed != 0 {
		t.Fatalf("browser closed while a capture was in flight: closed = %d", closed)
	}
	close(eng.block)
	<-done
}

func TestResolverBoundsConcurrency(t *testing.T) {
	var live, peak atomic.Int32
	release := make(chan struct{})
	resolver := newTestResolver(t, Options{MaxSessions: 2, IdleTimeout: time.Hour},
		func(context.Context, Options) (engine, error) {
			return &countingEngine{live: &live, peak: &peak, release: release}, nil
		})

	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = resolver.Capture(t.Context(), testRequest())
		}()
	}
	time.Sleep(60 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := peak.Load(); got > 2 {
		t.Fatalf("concurrent captures = %d, want at most 2", got)
	}
}

type countingEngine struct {
	live, peak *atomic.Int32
	release    chan struct{}
}

func (e *countingEngine) capture(ctx context.Context, _ Request) ([]Candidate, error) {
	current := e.live.Add(1)
	for {
		peak := e.peak.Load()
		if current <= peak || e.peak.CompareAndSwap(peak, current) {
			break
		}
	}
	defer e.live.Add(-1)
	select {
	case <-e.release:
	case <-ctx.Done():
	}
	return nil, ErrNoMedia
}

func (e *countingEngine) close() {}

func TestResolverHonoursCancellation(t *testing.T) {
	eng := &fakeEngine{block: make(chan struct{})}
	resolver := newTestResolver(t, Options{IdleTimeout: time.Hour},
		func(context.Context, Options) (engine, error) { return eng, nil })

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	_, err := resolver.Capture(ctx, testRequest())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Capture() error = %v, want context.Canceled", err)
	}
}

func TestResolverRestartsAfterBrowserCrash(t *testing.T) {
	crashed := &fakeEngine{err: engineFailure{errors.New("connection reset")}}
	healthy := &fakeEngine{result: []Candidate{{URL: "https://cdn.test/a.mp4"}}}
	var calls atomic.Int32
	resolver := newTestResolver(t, Options{IdleTimeout: time.Hour}, func(context.Context, Options) (engine, error) {
		if calls.Add(1) == 1 {
			return crashed, nil
		}
		return healthy, nil
	})

	if _, err := resolver.Capture(t.Context(), testRequest()); err == nil {
		t.Fatal("Capture() error = nil, want the crash to surface")
	}
	if _, closed := crashed.counts(); closed != 1 {
		t.Fatalf("crashed browser closed = %d, want 1", closed)
	}
	if _, err := resolver.Capture(t.Context(), testRequest()); err != nil {
		t.Fatalf("Capture() after crash error = %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("browser starts = %d, want a fresh process after the crash", got)
	}
}

func TestResolverKeepsBrowserAfterEmptyPage(t *testing.T) {
	eng := &fakeEngine{err: ErrNoMedia}
	var calls atomic.Int32
	resolver := newTestResolver(t, Options{IdleTimeout: time.Hour}, func(context.Context, Options) (engine, error) {
		calls.Add(1)
		return eng, nil
	})

	for range 2 {
		if _, err := resolver.Capture(t.Context(), testRequest()); !errors.Is(err, ErrNoMedia) {
			t.Fatalf("Capture() error = %v, want ErrNoMedia", err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("browser starts = %d, want the browser to survive an empty page", got)
	}
	if _, closed := eng.counts(); closed != 0 {
		t.Fatalf("browser closed = %d, want it kept alive", closed)
	}
}

func TestResolverCloseStopsBrowserAndRejectsWork(t *testing.T) {
	eng := &fakeEngine{}
	resolver := New(Options{IdleTimeout: time.Hour})
	resolver.newEngine = func(context.Context, Options) (engine, error) { return eng, nil }

	_, _ = resolver.Capture(t.Context(), testRequest())
	resolver.Close()
	resolver.Close()

	if _, closed := eng.counts(); closed != 1 {
		t.Fatalf("browser closed = %d, want exactly one shutdown", closed)
	}
	if _, err := resolver.Capture(t.Context(), testRequest()); !errors.Is(err, ErrClosed) {
		t.Fatalf("Capture() after Close() error = %v, want ErrClosed", err)
	}
}

func TestCaptureRejectsIncompleteRequests(t *testing.T) {
	resolver := newTestResolver(t, Options{}, func(context.Context, Options) (engine, error) {
		t.Fatal("browser started for an invalid request")
		return nil, nil
	})
	if _, err := resolver.Capture(t.Context(), Request{Accept: func(Candidate) bool { return true }}); err == nil {
		t.Fatal("Capture() without a page URL returned no error")
	}
	if _, err := resolver.Capture(t.Context(), Request{PageURL: "https://example.test"}); err == nil {
		t.Fatal("Capture() without an accept predicate returned no error")
	}
}

func TestIsProfileLocked(t *testing.T) {
	locked := errors.New(`[launcher] Failed to get the debug url: ERROR:process_singleton_posix.cc:347] ` +
		`Failed to create /tmp/profile/SingletonLock: File exists (17)`)
	if !isProfileLocked(locked) {
		t.Fatal("isProfileLocked() = false for a singleton lock failure")
	}
	if isProfileLocked(errors.New("exec: \"chromium\": executable file not found in $PATH")) {
		t.Fatal("isProfileLocked() = true for an unrelated launch failure")
	}
}

func TestResolverReportsLockedProfile(t *testing.T) {
	resolver := newTestResolver(t, Options{}, func(context.Context, Options) (engine, error) {
		return nil, ErrProfileLocked
	})
	if _, err := resolver.Capture(t.Context(), testRequest()); !errors.Is(err, ErrProfileLocked) {
		t.Fatalf("Capture() error = %v, want ErrProfileLocked", err)
	}
}

func TestCollectorAppliesSettleWindow(t *testing.T) {
	collector := newCollector(Request{
		PageURL: "https://example.test/watch",
		Settle:  50 * time.Millisecond,
		Accept:  func(c Candidate) bool { return c.Kind == "Media" },
	})

	if _, done := collector.settled(); done {
		t.Fatal("collector settled before anything matched")
	}
	collector.request(&proto.NetworkRequestWillBeSent{
		RequestID: "1", Type: "Media",
		Request: &proto.NetworkRequest{URL: "https://cdn.test/a.mp4", Headers: proto.NetworkHeaders{}},
	})
	collector.response(&proto.NetworkResponseReceived{
		RequestID: "1", Type: "Media", Response: &proto.NetworkResponse{Status: 200},
	})
	if _, done := collector.settled(); done {
		t.Fatal("collector settled before the settle window elapsed")
	}

	collector.request(&proto.NetworkRequestWillBeSent{
		RequestID: "2", Type: "Image",
		Request: &proto.NetworkRequest{URL: "https://cdn.test/cover.png", Headers: proto.NetworkHeaders{}},
	})
	time.Sleep(60 * time.Millisecond)

	found, done := collector.settled()
	if !done {
		t.Fatal("collector did not settle after the window elapsed")
	}
	if len(found) != 1 || found[0].URL != "https://cdn.test/a.mp4" {
		t.Fatalf("collector matches = %#v, want only the media request", found)
	}
}

type captureEventCDP struct {
	lifecycleCDP
	events chan *cdp.Event
}

func (c captureEventCDP) Event() <-chan *cdp.Event { return c.events }

func TestChromeCaptureHonorsContextErrorsWithPartialMatches(t *testing.T) {
	for _, want := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(want.Error(), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			client := captureEventCDP{events: make(chan *cdp.Event, 2)}
			client.call = func(_ context.Context, method string, _ interface{}) ([]byte, error) {
				switch method {
				case "Target.createTarget":
					return []byte(`{"targetId":"tab"}`), nil
				case "Target.attachToTarget":
					return []byte(`{"sessionId":"session"}`), nil
				case "Page.navigate":
					client.events <- &cdp.Event{SessionID: "session", Method: "Network.requestWillBeSent", Params: []byte(`{"requestId":"media","request":{"url":"https://cdn.test/video.mp4"}}`)}
					client.events <- &cdp.Event{SessionID: "session", Method: "Network.responseReceived", Params: []byte(`{"requestId":"media","response":{"status":200}}`)}
				}
				return []byte(`{}`), nil
			}
			b := rod.New().Context(t.Context()).Client(client).NoDefaultDevice()
			if err := b.Connect(); err != nil {
				t.Fatal(err)
			}
			var ready atomic.Bool
			req := testRequest()
			req.Settle = time.Hour
			req.Ready = func(Candidate) bool {
				ready.Store(true)
				if want == context.Canceled {
					cancel()
				}
				return true
			}
			found, err := (&chromeEngine{browser: b}).capture(ctx, req)
			if !ready.Load() {
				t.Fatal("capture ended before a partial match was accepted")
			}
			if !errors.Is(err, want) || len(found) != 0 {
				t.Fatalf("capture() = %#v, %v; want no matches and %v", found, err, want)
			}
		})
	}
}

func TestCollectorRecordsResponseDetail(t *testing.T) {
	collector := newCollector(Request{PageURL: "https://example.test/watch", Accept: func(Candidate) bool { return true }})
	collector.request(&proto.NetworkRequestWillBeSent{
		RequestID: "1", Type: "Media",
		Request: &proto.NetworkRequest{
			URL:     "https://cdn.test/a.mp4",
			Headers: proto.NetworkHeaders{"Referer": gson.New("https://player.test/")},
		},
	})
	collector.response(&proto.NetworkResponseReceived{
		RequestID: "1", Type: "Media",
		Response: &proto.NetworkResponse{Status: 206, MIMEType: "video/mp4"},
	})

	found := collector.matches()
	if len(found) != 1 {
		t.Fatalf("matches = %d, want 1", len(found))
	}
	if found[0].Status != 206 || found[0].MIME != "video/mp4" {
		t.Fatalf("candidate = %#v, want the response detail merged in", found[0])
	}
	if got := found[0].Headers["Referer"]; got != "https://player.test/" {
		t.Fatalf("Referer = %q, want the header Chromium sent", got)
	}
}

func TestCollectorDetectsBotChallenge(t *testing.T) {
	page := "https://example.test/watch"
	collector := newCollector(Request{PageURL: page, Accept: func(Candidate) bool { return false }})
	collector.request(&proto.NetworkRequestWillBeSent{
		RequestID: "1", Type: "Document",
		Request: &proto.NetworkRequest{URL: page, Headers: proto.NetworkHeaders{}},
	})
	collector.response(&proto.NetworkResponseReceived{
		RequestID: "1", Type: "Document",
		Response: &proto.NetworkResponse{Status: 403, URL: page},
	})
	if !collector.challenged() {
		t.Fatal("collector did not report the bot challenge")
	}
}

func TestCollectorIgnoresSubResourceFailures(t *testing.T) {
	page := "https://example.test/watch"
	collector := newCollector(Request{PageURL: page, Accept: func(Candidate) bool { return false }})
	collector.request(&proto.NetworkRequestWillBeSent{
		RequestID: "1", Type: "XHR",
		Request: &proto.NetworkRequest{URL: "https://ads.test/beacon", Headers: proto.NetworkHeaders{}},
	})
	collector.response(&proto.NetworkResponseReceived{
		RequestID: "1", Type: "XHR",
		Response: &proto.NetworkResponse{Status: 403, URL: "https://ads.test/beacon"},
	})
	if collector.challenged() {
		t.Fatal("a blocked sub-resource was mistaken for a bot challenge")
	}
}
