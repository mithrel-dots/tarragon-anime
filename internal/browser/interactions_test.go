package browser

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
	"tarragon-anime/internal/streamlog"
)

func exhaustedCollector() *collector {
	col := newCollector(testRequest())
	col.loadedAt = time.Now().Add(-noMediaGrace - time.Second)
	return col
}

func TestInteractionWaitHonorsCancellation(t *testing.T) {
	for _, occupied := range []bool{false, true} {
		t.Run(map[bool]string{false: "free", true: "occupied"}[occupied], func(t *testing.T) {
			e := &chromeEngine{interactions: make(chan struct{}, 1)}
			if occupied {
				e.interactions <- struct{}{}
			}
			copy := *e
			ctx, cancel := context.WithCancelCause(t.Context())
			cancel(streamlog.ErrWinnerSelected)
			// A nil page proves no focus/DOM work happens after cancellation,
			// including when select chooses a free gate over the ready Done channel.
			if err := copy.withInteraction(ctx, nil, exhaustedCollector(), func() error { t.Error("cancelled click ran"); return nil }); !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if occupied {
				ctx, stop := context.WithTimeout(t.Context(), 20*time.Millisecond)
				defer stop()
				if err := copy.withInteraction(ctx, nil, exhaustedCollector(), func() error { t.Error("timed out click ran"); return nil }); !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal(err)
				}
				if len(e.interactions) != 1 {
					t.Fatal("waiting cancellation released another page's gate")
				}
				<-e.interactions
			}
			if len(e.interactions) != 0 {
				t.Fatal("cancelled acquisition leaked the gate")
			}
		})
	}
}

func TestInteractionWaitYieldsToLateMediaAndChallenge(t *testing.T) {
	for _, challenge := range []bool{false, true} {
		t.Run(map[bool]string{false: "media", true: "challenge"}[challenge], func(t *testing.T) {
			e := &chromeEngine{interactions: make(chan struct{}, 1)}
			e.interactions <- struct{}{}
			col := exhaustedCollector()
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			done := make(chan error, 1)
			click := func() error { t.Error("late media/challenge was ignored"); return nil }
			go func() { done <- e.withInteraction(ctx, nil, col, click) }()
			if challenge {
				col.response(&proto.NetworkResponseReceived{Type: proto.NetworkResourceTypeDocument, Response: &proto.NetworkResponse{URL: col.req.PageURL, Status: 403}})
			} else {
				col.request(&proto.NetworkRequestWillBeSent{RequestID: "media", Request: &proto.NetworkRequest{URL: "https://fixture.test/episode.mp4"}})
				col.response(&proto.NetworkResponseReceived{RequestID: "media", Response: &proto.NetworkResponse{Status: 206}})
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if len(e.interactions) != 1 {
				t.Fatal("observation released another page's gate")
			}
			<-e.interactions
			// The same checks must also happen after acquiring a now-free gate.
			if err := e.withInteraction(ctx, nil, col, click); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestInteractionCopiesSerializeAndReleaseOnError(t *testing.T) {
	var focuses atomic.Int32
	client := lifecycleCDP{call: func(ctx context.Context, method string, _ interface{}) ([]byte, error) {
		if method == "Target.attachToTarget" {
			return []byte(`{"sessionId":"root-session"}`), nil
		}
		if method == "Page.bringToFront" {
			if _, ok := ctx.Deadline(); !ok {
				t.Error("foreground operation lost the capture deadline")
			}
			focuses.Add(1)
		}
		return []byte(`{}`), nil
	}}
	b := rod.New().Context(t.Context()).Client(client).NoDefaultDevice()
	if err := b.Connect(); err != nil {
		t.Fatal(err)
	}
	root, err := b.PageFromTarget("root")
	if err != nil {
		t.Fatal(err)
	}
	e := &chromeEngine{interactions: make(chan struct{}, 1)}
	copy := *e
	ctx, stop := context.WithTimeout(t.Context(), time.Second)
	defer stop()
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	failed := errors.New("click failed")
	done := make(chan error, 1)
	go func() {
		done <- e.withInteraction(ctx, root, exhaustedCollector(), func() error { close(entered); <-release; return failed })
	}()
	<-entered
	wait, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	waiting := make(chan error, 1)
	go func() {
		waiting <- copy.withInteraction(wait, root, exhaustedCollector(), func() error { t.Error("overlapping clicks"); return nil })
	}()
	cancel(streamlog.ErrWinnerSelected)
	if err := <-waiting; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if focuses.Load() != 1 {
		t.Fatal("waiting copy stole foreground input")
	}
	release <- struct{}{}
	if err := <-done; !errors.Is(err, failed) {
		t.Fatal(err)
	}
	if err := copy.withInteraction(ctx, root, exhaustedCollector(), func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if focuses.Load() != 2 || len(e.interactions) != 0 {
		t.Fatal("failed click did not release the shared gate")
	}
}
