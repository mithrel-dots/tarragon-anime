package browser

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/cdp"
	"github.com/go-rod/rod/lib/proto"
)

func TestCaptureTimeoutIncludesStartupAndQueue(t *testing.T) {
	for _, queued := range []bool{false, true} {
		t.Run(map[bool]string{false: "startup", true: "queue"}[queued], func(t *testing.T) {
			var starts atomic.Int32
			resolver := newTestResolver(t, Options{Timeout: 20 * time.Millisecond}, func(ctx context.Context, _ Options) (engine, error) {
				starts.Add(1)
				<-ctx.Done()
				return nil, ctx.Err()
			})
			if queued {
				for range cap(resolver.sem) {
					resolver.sem <- struct{}{}
				}
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			start := time.Now()
			_, err := resolver.Capture(ctx, testRequest())
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("Capture() = %v, want deadline exceeded", err)
			}
			if time.Since(start) > 500*time.Millisecond {
				t.Fatal("capture timeout did not bound startup/queue wait")
			}
			if queued && starts.Load() != 0 {
				t.Fatal("started browser while all session slots were occupied")
			}
		})
	}
}

func TestStartupWaitCanBeCancelledAndClosed(t *testing.T) {
	started := make(chan struct{})
	finish := make(chan struct{})
	defer close(finish)
	eng := &fakeEngine{}
	resolver := newTestResolver(t, Options{}, func(ctx context.Context, _ Options) (engine, error) {
		close(started)
		<-finish // Simulate slow startup cleanup even after cancellation.
		return eng, nil
	})
	first := make(chan error, 1)
	go func() {
		_, err := resolver.Capture(t.Context(), testRequest())
		first <- err
	}()
	<-started
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	waiter := make(chan error, 1)
	go func() {
		_, err := resolver.Capture(ctx, testRequest())
		waiter <- err
	}()
	select {
	case err := <-waiter:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("waiting capture = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("waiting capture blocked on startup lock")
	}
	closed := make(chan struct{})
	go func() { resolver.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close blocked on startup")
	}
	select {
	case err := <-first:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("starting capture = %v, want ErrClosed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("starting capture did not observe Close")
	}
	resolver.mu.Lock()
	startup := resolver.starting
	resolver.mu.Unlock()
	// Release startup and wait for disposal before test cleanup.
	finish <- struct{}{}
	<-startup
	if _, closed := eng.counts(); closed != 1 {
		t.Fatalf("late engine closed %d times, want 1", closed)
	}
}

func TestCancelledCaptureDoesNotDiscardSharedEngine(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		eng := &fakeEngine{err: engineFailure{cause}}
		var starts atomic.Int32
		resolver := newTestResolver(t, Options{}, func(context.Context, Options) (engine, error) {
			starts.Add(1)
			return eng, nil
		})
		for range 2 {
			if _, err := resolver.Capture(t.Context(), testRequest()); !errors.Is(err, cause) {
				t.Fatalf("Capture() = %v, want %v", err, cause)
			}
		}
		if starts.Load() != 1 {
			t.Fatal("page cancellation restarted the shared engine")
		}
	}
}

func TestCancelledStartupDisposesLateEngineAndRetries(t *testing.T) {
	started := make(chan struct{})
	finish := make(chan struct{})
	defer close(finish)
	late, healthy := &fakeEngine{}, &fakeEngine{}
	var starts atomic.Int32
	resolver := newTestResolver(t, Options{}, func(ctx context.Context, _ Options) (engine, error) {
		if starts.Add(1) == 1 {
			close(started)
			<-ctx.Done()
			<-finish
			return late, nil
		}
		return healthy, nil
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := resolver.Capture(ctx, testRequest())
		done <- err
	}()
	<-started
	resolver.mu.Lock()
	startup := resolver.starting
	resolver.mu.Unlock()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Capture() = %v, want cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Capture waited for startup cleanup after cancellation")
	}
	finish <- struct{}{}
	<-startup
	if captures, closed := late.counts(); captures != 0 || closed != 1 {
		t.Fatalf("late engine captures=%d closed=%d, want 0 and 1", captures, closed)
	}
	for range 2 {
		if _, err := resolver.Capture(t.Context(), testRequest()); err != nil {
			t.Fatal(err)
		}
	}
	if starts.Load() != 2 {
		t.Fatalf("browser starts = %d, want one retry reused by later captures", starts.Load())
	}
}

func TestNewChromeEngineRejectsCancelledStartup(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := newChromeEngine(ctx, Options{Binary: "/does/not/exist"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("newChromeEngine() = %v, want cancellation before launch", err)
	}
}

type lifecycleCDP struct {
	call func(context.Context, string, interface{}) ([]byte, error)
}

func (c lifecycleCDP) Event() <-chan *cdp.Event { return nil }
func (c lifecycleCDP) Call(ctx context.Context, _, method string, params interface{}) ([]byte, error) {
	return c.call(ctx, method, params)
}

func TestCaptureClosesTargetAfterCancellation(t *testing.T) {
	for _, stage := range []string{"Target.attachToTarget", "Network.enable", "Page.navigate"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			closed := false
			client := lifecycleCDP{call: func(callCtx context.Context, method string, params interface{}) ([]byte, error) {
				if method == stage {
					cancel()
					return nil, callCtx.Err()
				}
				switch method {
				case "Target.createTarget":
					return []byte(`{"targetId":"tab"}`), nil
				case "Target.attachToTarget":
					return []byte(`{"sessionId":"session"}`), nil
				case "Target.closeTarget":
					if callCtx.Err() != nil {
						t.Error("cleanup inherited cancelled page context")
					}
					if deadline, ok := callCtx.Deadline(); !ok || time.Until(deadline) > 2*time.Second {
						t.Error("cleanup must have a bounded deadline")
					}
					if params.(proto.TargetCloseTarget).TargetID != "tab" {
						t.Error("cleanup closed the wrong target")
					}
					closed = true
				}
				return []byte(`{}`), nil
			}}
			b := rod.New().Context(t.Context()).Client(client).NoDefaultDevice()
			if err := b.Connect(); err != nil {
				t.Fatal(err)
			}
			eng := &chromeEngine{browser: b}
			if _, err := eng.capture(ctx, testRequest()); !errors.Is(err, context.Canceled) {
				t.Fatalf("capture() = %v, want cancellation", err)
			}
			if !closed {
				t.Fatal("cancelled capture leaked its target")
			}
		})
	}
}
