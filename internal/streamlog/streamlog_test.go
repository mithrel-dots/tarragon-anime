package streamlog_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"tarragon-anime/internal/streamlog"
)

type synchronizedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *synchronizedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *synchronizedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func records(t *testing.T, buf *synchronizedBuffer) []map[string]string {
	t.Helper()
	output := buf.String()
	if output == "" {
		return nil
	}
	var result []map[string]string
	for _, line := range strings.Split(strings.TrimSuffix(output, "\n"), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 11 {
			t.Fatalf("malformed log record: %q", line)
		}
		record := map[string]string{"event": fields[0]}
		for _, field := range fields[1:] {
			key, value, ok := strings.Cut(field, "=")
			if !ok || value == "" || record[key] != "" {
				t.Fatalf("malformed or duplicate field: %q", field)
			}
			record[key] = value
		}
		for _, key := range []string{"resolution", "media_id", "episode", "translation", "provider", "source", "stage", "status", "reason", "elapsed_ms"} {
			if record[key] == "" {
				t.Fatalf("missing %s in record: %v", key, record)
			}
		}
		if elapsed, err := strconv.ParseFloat(record["elapsed_ms"], 64); err != nil || !(elapsed >= 0) {
			t.Fatalf("invalid elapsed_ms: %q", record["elapsed_ms"])
		}
		result = append(result, record)
	}
	return result
}

func assertFields(t *testing.T, record, want map[string]string) {
	t.Helper()
	for key, value := range want {
		if record[key] != value {
			t.Errorf("%s = %q, want %q (record: %v)", key, record[key], value, record)
		}
	}
}

func terminalRecord(t *testing.T, entries []map[string]string) map[string]string {
	t.Helper()
	if len(entries) != 2 {
		t.Fatalf("got %d records, want exactly one start and one terminal: %v", len(entries), entries)
	}
	assertFields(t, entries[0], map[string]string{"event": "stream_attempt", "status": "started", "reason": "-", "elapsed_ms": "0.000"})
	assertFields(t, entries[1], map[string]string{"event": "stream_attempt"})
	for _, key := range []string{"resolution", "media_id", "episode", "translation", "provider", "source", "stage"} {
		if entries[0][key] != entries[1][key] {
			t.Errorf("%s changed between start and terminal: %v", key, entries)
		}
	}
	if entries[1]["status"] == "started" {
		t.Fatal("terminal record has started status")
	}
	return entries[1]
}

func TestConcurrentFinishAndCancel(t *testing.T) {
	for _, tt := range []struct {
		name, reason string
		cause        error
	}{
		{name: "finish_only"},
		{name: "caller_cancel", cause: context.Canceled, reason: "request_cancelled"},
		{name: "winner_selected", cause: streamlog.ErrWinnerSelected, reason: "winner_selected"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				for range 32 {
					var buf synchronizedBuffer
					ctx, cancel := context.WithCancelCause(streamlog.WithResolution(context.Background(), log.New(&buf, "", 0), 123, 4, "sub"))
					finish := streamlog.Start(ctx, "primary", "resolve")
					ready := make(chan struct{})
					var workers sync.WaitGroup
					for range 8 {
						workers.Go(func() {
							<-ready
							finish("resolved", "")
							finish("failed", "duplicate_finish")
						})
					}
					if tt.cause != nil {
						workers.Go(func() {
							<-ready
							cancel(tt.cause)
							cancel(errors.New("ignored second cause"))
						})
					}
					close(ready)
					workers.Wait()
					cancel(nil)
					synctest.Wait()
					terminal := terminalRecord(t, records(t, &buf))
					// Either contender may win; only the terminal count is deterministic.
					if terminal["status"] == "resolved" {
						assertFields(t, terminal, map[string]string{"reason": "-"})
					} else if tt.cause != nil {
						assertFields(t, terminal, map[string]string{"status": "cancelled", "reason": tt.reason})
					} else {
						t.Fatalf("unexpected terminal outcome: %v", terminal)
					}
				}
			})
		})
	}
}

func TestCancellationCompletesAttempt(t *testing.T) {
	for _, tt := range []struct {
		name, status, reason string
		cause                error
		deadline, before     bool
		finishBeforeWait     bool
	}{
		{name: "caller", cause: context.Canceled, status: "cancelled", reason: "request_cancelled"},
		{name: "winner", cause: streamlog.ErrWinnerSelected, status: "cancelled", reason: "winner_selected"},
		{name: "wrapped_winner", cause: fmt.Errorf("https://example.invalid/?signature=SECRET: %w", streamlog.ErrWinnerSelected), status: "cancelled", reason: "winner_selected"},
		{name: "unsafe_cause", cause: errors.New("Cookie: session=SECRET\nforged=record"), status: "cancelled", reason: "request_cancelled"},
		{name: "deadline", deadline: true, status: "timeout", reason: "deadline_exceeded"},
		{name: "already_cancelled", before: true, cause: context.Canceled, status: "cancelled", reason: "request_cancelled"},
		{name: "already_expired", before: true, deadline: true, status: "timeout", reason: "deadline_exceeded"},
		{name: "finish_after_caller_cancel", finishBeforeWait: true, cause: context.Canceled, status: "cancelled", reason: "request_cancelled"},
		{name: "finish_after_winner_cancel", finishBeforeWait: true, cause: streamlog.ErrWinnerSelected, status: "cancelled", reason: "winner_selected"},
		{name: "finish_after_deadline", finishBeforeWait: true, deadline: true, status: "timeout", reason: "deadline_exceeded"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var buf synchronizedBuffer
				ctx, cancel := context.WithCancelCause(streamlog.WithResolution(context.Background(), log.New(&buf, "", 0), 123, 4, "sub"))
				defer cancel(nil)
				trigger := func() { cancel(tt.cause) }
				if tt.deadline {
					var stop context.CancelFunc
					ctx, stop = context.WithTimeout(ctx, time.Second)
					defer stop()
					trigger = func() { <-ctx.Done() }
				}
				if tt.before {
					trigger()
				}
				finish := streamlog.Start(ctx, "primary", "resolve")
				if !tt.before {
					trigger()
				}
				if tt.finishBeforeWait {
					finish("resolved", "")
				}
				synctest.Wait()
				want := map[string]string{"status": tt.status, "reason": tt.reason}
				assertFields(t, terminalRecord(t, records(t, &buf)), want)
				finish("resolved", "")
				finish("failed", "late_finish")
				cancel(nil)
				synctest.Wait()
				assertFields(t, terminalRecord(t, records(t, &buf)), want)
			})
		})
	}
}

func TestSuccessfulFinishBeforeCancellationStaysResolved(t *testing.T) {
	for _, cause := range []error{context.Canceled, streamlog.ErrWinnerSelected, context.DeadlineExceeded} {
		t.Run(cause.Error(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var buf synchronizedBuffer
				ctx, cancel := context.WithCancelCause(streamlog.WithResolution(context.Background(), log.New(&buf, "", 0), 123, 4, "sub"))
				defer cancel(nil)
				if cause == context.DeadlineExceeded {
					var stop context.CancelFunc
					ctx, stop = context.WithTimeout(ctx, time.Second)
					defer stop()
				}
				finish := streamlog.Start(ctx, "primary", "resolve")
				finish("resolved", "")
				if cause == context.DeadlineExceeded {
					<-ctx.Done()
				} else {
					cancel(cause)
				}
				synctest.Wait()
				finish("cancelled", "late_finish")
				assertFields(t, terminalRecord(t, records(t, &buf)), map[string]string{"status": "resolved", "reason": "-"})
			})
		})
	}
}

func TestOutcome(t *testing.T) {
	for _, tt := range []struct {
		name, status, reason string
		err, cause           error
		deadline             bool
	}{
		{name: "success", status: "resolved"},
		{name: "signed_url", err: errors.New("https://example.invalid/video?signature=SECRET"), status: "failed", reason: "resolution_failed"},
		{name: "cookie", err: errors.New("Cookie: session=SECRET"), status: "failed", reason: "resolution_failed"},
		{name: "newline", err: errors.New("upstream failure\nstatus=resolved token=SECRET"), status: "failed", reason: "resolution_failed"},
		{name: "cancelled_error", err: context.Canceled, status: "cancelled", reason: "request_cancelled"},
		{name: "wrapped_cancelled_error", err: fmt.Errorf("Cookie: session=SECRET: %w", context.Canceled), status: "cancelled", reason: "request_cancelled"},
		{name: "deadline_error", err: context.DeadlineExceeded, status: "timeout", reason: "deadline_exceeded"},
		{name: "wrapped_deadline_error", err: fmt.Errorf("signed URL SECRET: %w", context.DeadlineExceeded), status: "timeout", reason: "deadline_exceeded"},
		{name: "caller_context", cause: context.Canceled, status: "cancelled", reason: "request_cancelled"},
		{name: "unsafe_cause", cause: errors.New("Cookie: session=SECRET\nforged=record"), status: "cancelled", reason: "request_cancelled"},
		{name: "deadline_context", deadline: true, status: "timeout", reason: "deadline_exceeded"},
		{name: "winner_context", cause: streamlog.ErrWinnerSelected, status: "cancelled", reason: "winner_selected"},
		{name: "winner_over_error", cause: streamlog.ErrWinnerSelected, err: context.DeadlineExceeded, status: "cancelled", reason: "winner_selected"},
		{name: "wrapped_winner", cause: fmt.Errorf("SECRET: %w", streamlog.ErrWinnerSelected), status: "cancelled", reason: "winner_selected"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var buf synchronizedBuffer
			ctx, cancel := context.WithCancelCause(streamlog.WithResolution(context.Background(), log.New(&buf, "", 0), 123, 4, "sub"))
			defer cancel(nil)
			if tt.cause != nil {
				cancel(tt.cause)
			}
			if tt.deadline {
				var stop context.CancelFunc
				ctx, stop = context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer stop()
			}
			status, reason := streamlog.Outcome(ctx, tt.err)
			if status != tt.status || reason != tt.reason {
				t.Fatalf("Outcome = (%q, %q), want (%q, %q)", status, reason, tt.status, tt.reason)
			}
			finish := streamlog.Start(ctx, "primary", "resolve")
			finish(status, reason)
			loggedReason := tt.reason
			if loggedReason == "" {
				loggedReason = "-"
			}
			assertFields(t, terminalRecord(t, records(t, &buf)), map[string]string{"status": tt.status, "reason": loggedReason})
			if strings.Contains(buf.String(), "SECRET") {
				t.Fatal("error text leaked into log output")
			}
		})
	}
}

func TestProviderContextsAreIsolatedAndShareResolution(t *testing.T) {
	var buf synchronizedBuffer
	base := streamlog.WithResolution(context.Background(), log.New(&buf, "", 0), 123, 4, "sub")
	left := streamlog.WithProvider(base, "left")
	contexts := map[string]context.Context{
		"base": base, "left": left, "right": streamlog.WithProvider(base, "right"), "nested": streamlog.WithProvider(left, "nested"),
	}
	var workers sync.WaitGroup
	for source, ctx := range contexts {
		workers.Go(func() {
			finish := streamlog.Start(ctx, source, "resolve")
			finish("resolved", "")
			streamlog.Selected(ctx, source)
		})
	}
	workers.Wait()
	entries := records(t, &buf)
	if len(entries) != 3*len(contexts) {
		t.Fatalf("got %d records, want %d", len(entries), 3*len(contexts))
	}
	bySource := make(map[string][]map[string]string)
	for _, entry := range entries {
		provider := entry["source"]
		if provider == "base" {
			provider = "-"
		}
		assertFields(t, entry, map[string]string{"resolution": entries[0]["resolution"], "provider": provider, "media_id": "123", "episode": "4", "translation": "sub"})
		bySource[entry["source"]] = append(bySource[entry["source"]], entry)
	}
	for source := range contexts {
		entries := bySource[source]
		if len(entries) != 3 {
			t.Fatalf("source %s has %d records, want 3", source, len(entries))
		}
		terminalRecord(t, entries[:2])
		assertFields(t, entries[2], map[string]string{"event": "stream_selected", "stage": "select", "status": "resolved", "reason": "-"})
		selectedElapsed, _ := strconv.ParseFloat(entries[2]["elapsed_ms"], 64)
		attemptElapsed, _ := strconv.ParseFloat(entries[1]["elapsed_ms"], 64)
		if selectedElapsed < attemptElapsed {
			t.Fatalf("selection elapsed=%v excludes attempt elapsed=%v", selectedElapsed, attemptElapsed)
		}
	}
}

func TestConcurrentResolutionIDsAreUnique(t *testing.T) {
	const count = 256
	var buf synchronizedBuffer
	logger := log.New(&buf, "", 0)
	ready := make(chan struct{})
	var workers sync.WaitGroup
	for i := range count {
		workers.Go(func() {
			<-ready
			ctx := streamlog.WithResolution(context.Background(), logger, i+1, 4, "sub")
			finish := streamlog.Start(streamlog.WithProvider(ctx, "provider"), "primary", "resolve")
			finish("resolved", "")
		})
	}
	close(ready)
	workers.Wait()
	byID := make(map[string][]map[string]string)
	for _, entry := range records(t, &buf) {
		byID[entry["resolution"]] = append(byID[entry["resolution"]], entry)
	}
	if len(byID) != count {
		t.Fatalf("got %d unique resolution IDs, want %d", len(byID), count)
	}
	for id, entries := range byID {
		if id == "-" || id == "redacted" {
			t.Fatalf("invalid resolution ID %q", id)
		}
		assertFields(t, terminalRecord(t, entries), map[string]string{"status": "resolved", "provider": "provider"})
	}
}

func TestNoTraceAndNilLoggerAreNoOps(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		base, cancel := context.WithCancel(context.Background())
		defer cancel()
		if got := streamlog.WithProvider(base, "provider"); got != base {
			t.Fatal("WithProvider changed an untraced context")
		}
		for _, ctx := range []context.Context{base, streamlog.WithProvider(streamlog.WithResolution(base, nil, 123, 4, "sub"), "provider")} {
			finish := streamlog.Start(ctx, "primary", "resolve")
			finish("resolved", "")
			cancel()
			finish("failed", "late_finish")
			streamlog.Selected(ctx, "primary")
		}
		synctest.Wait()
	})
}

func TestIdentifierFormatting(t *testing.T) {
	for _, tt := range []struct{ name, value, want string }{
		{"empty", "", "-"},
		{"identifier", "Provider_1.source-2", "Provider_1.source-2"},
		{"maximum_length", strings.Repeat("a", 64), strings.Repeat("a", 64)},
		{"too_long", strings.Repeat("a", 65), "redacted"},
		{"signed_url", "https://example.invalid/video?signature=SECRET", "redacted"},
		{"cookie", "Cookie: session=SECRET", "redacted"},
		{"newline", "safe\nstatus=resolved token=SECRET", "redacted"},
		{"carriage_return", "safe\rSECRET", "redacted"},
		{"raw_error", errors.New("upstream rejected SECRET credential").Error(), "redacted"},
		{"tab", "safe\tSECRET", "redacted"},
		{"nul", "safe\x00SECRET", "redacted"},
		{"unicode", "s\u00fcb", "redacted"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var buf synchronizedBuffer
			ctx := streamlog.WithProvider(streamlog.WithResolution(context.Background(), log.New(&buf, "", 0), 123, 4, tt.value), tt.value)
			finish := streamlog.Start(ctx, tt.value, tt.value)
			finish(tt.value, tt.value)
			streamlog.Selected(ctx, tt.value)
			entries := records(t, &buf)
			if len(entries) != 3 {
				t.Fatalf("got %d records, want 3", len(entries))
			}
			for _, entry := range entries {
				assertFields(t, entry, map[string]string{"translation": tt.want, "provider": tt.want, "source": tt.want})
			}
			assertFields(t, entries[0], map[string]string{"stage": tt.want, "status": "started", "reason": "-"})
			assertFields(t, terminalRecord(t, entries[:2]), map[string]string{"stage": tt.want, "status": tt.want, "reason": tt.want})
			assertFields(t, entries[2], map[string]string{"event": "stream_selected", "stage": "select", "status": "resolved", "reason": "-"})
			if strings.Contains(buf.String(), "SECRET") {
				t.Fatal("secret leaked into log output")
			}
		})
	}
}
