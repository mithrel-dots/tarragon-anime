package browser

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"tarragon-anime/internal/streamlog"
)

type outcomeLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *outcomeLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *outcomeLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func traceCapture(ctx context.Context, logs *outcomeLog) context.Context {
	return streamlog.WithProvider(streamlog.WithResolution(ctx, log.New(logs, "", 0), 42, 7, "sub"), "fixture")
}

func assertSourceOutcomes(t *testing.T, logs string, want map[string]string) {
	t.Helper()
	starts, ends := map[string]int{}, map[string]int{}
	for _, line := range strings.Split(logs, "\n") {
		fields := map[string]string{}
		for _, field := range strings.Fields(line) {
			key, value, _ := strings.Cut(field, "=")
			fields[key] = value
		}
		if fields["stage"] != "" {
			switch fields["status"] {
			case "started", "resolved", "failed", "unavailable", "timeout", "cancelled":
			default:
				t.Fatalf("unsupported attempt status: %s", line)
			}
		}
		if fields["stage"] != "browser_source" {
			continue
		}
		source, status := fields["source"], fields["status"]
		if fields["resolution"] == "" || fields["provider"] != "fixture" || fields["media_id"] != "42" || fields["episode"] != "7" {
			t.Fatalf("missing trace identity: %s", line)
		}
		if _, ok := want[source]; !ok {
			t.Fatalf("unexpected source: %s", line)
		}
		if status == "started" {
			starts[source]++
		} else {
			ends[source]++
			if status != want[source] {
				t.Fatalf("source outcome: %s; want %s", line, want[source])
			}
		}
	}
	for source := range want {
		if starts[source] != 1 || ends[source] != 1 {
			t.Fatalf("source %s: starts=%d terminals=%d\n%s", source, starts[source], ends[source], logs)
		}
	}
}

func TestSourceIDsAreAllowlisted(t *testing.T) {
	for _, raw := range []string{"", "default", "source-1", "source-2", "source-3", "source-4", "source-01", "private-token", "https://cdn.test/?token=secret", "Authorization: secret", "DOM source text"} {
		want := "default"
		if raw == "source-1" || raw == "source-2" || raw == "source-3" {
			want = raw
		}
		if got := SourceID(raw); got != want {
			t.Fatalf("SourceID(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestSourceTerminalOutcomes(t *testing.T) {
	for _, test := range []struct {
		err            error
		status, reason string
	}{
		{ErrNoMedia, "unavailable", "no_matching_media"},
		{ErrClickFailed, "failed", "button_interaction_failed"},
		{ErrChallenged, "unavailable", "clearance_required"},
		{ErrClearancePending, "unavailable", "clearance_required"},
		{context.DeadlineExceeded, "timeout", "deadline_exceeded"},
		{context.Canceled, "cancelled", "request_cancelled"},
	} {
		t.Run(test.reason, func(t *testing.T) {
			var logs outcomeLog
			client := lifecycleCDP{call: func(_ context.Context, method string, _ interface{}) ([]byte, error) {
				if method == "Target.createTarget" {
					return nil, errors.Join(test.err, errors.New("https://cdn.test/?token=private-token"))
				}
				return []byte(`{}`), nil
			}}
			b := rod.New().Context(t.Context()).Client(client).NoDefaultDevice()
			if err := b.Connect(); err != nil {
				t.Fatal(err)
			}
			eng := &chromeEngine{browser: b}
			_, _ = eng.capture(traceCapture(t.Context(), &logs), testRequest())
			assertSourceOutcomes(t, logs.String(), map[string]string{"default": test.status})
			if !strings.Contains(logs.String(), "status="+test.status+" reason="+test.reason+" ") {
				t.Fatalf("incorrect terminal reason: %s", logs.String())
			}
			if strings.Contains(logs.String(), "private-token") || strings.Contains(logs.String(), "cdn.test") {
				t.Fatal("raw CDP error leaked into source outcomes")
			}
		})
	}
}

func TestSourceCancellationLogsBeforeLateWorkerReturns(t *testing.T) {
	var logs outcomeLog
	started, release := make(chan struct{}), make(chan struct{})
	client := lifecycleCDP{call: func(_ context.Context, method string, _ interface{}) ([]byte, error) {
		if method == "Target.createTarget" {
			close(started)
			<-release
			return nil, errors.New("late private-token failure")
		}
		return []byte(`{}`), nil
	}}
	b := rod.New().Context(t.Context()).Client(client).NoDefaultDevice()
	if err := b.Connect(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancelCause(traceCapture(t.Context(), &logs))
	defer cancel(nil)
	done := make(chan error, 1)
	go func() { _, err := (&chromeEngine{browser: b}).capture(ctx, testRequest()); done <- err }()
	<-started
	cancel(streamlog.ErrWinnerSelected)
	deadline := time.Now().Add(time.Second)
	for !strings.Contains(logs.String(), "reason=winner_selected") && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	before := logs.String()
	close(release)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	assertSourceOutcomes(t, before, map[string]string{"default": "cancelled"})
	if logs.String() != before {
		t.Fatal("late worker duplicated its terminal outcome")
	}
}

func TestCancelledRawCDPFailureKeepsSharedEngine(t *testing.T) {
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	eng := &fakeEngine{hook: func() error {
		cancel(streamlog.ErrWinnerSelected)
		return engineFailure{errors.New("target closed")}
	}}
	r := newTestResolver(t, Options{}, func(context.Context, Options) (engine, error) { return eng, nil })
	if _, err := r.Capture(ctx, testRequest()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	eng.hook = nil
	if _, err := r.Capture(t.Context(), testRequest()); err != nil {
		t.Fatal(err)
	}
	if captures, closed := eng.counts(); captures != 2 || closed != 0 {
		t.Fatalf("shared engine captures=%d closed=%d", captures, closed)
	}
}
