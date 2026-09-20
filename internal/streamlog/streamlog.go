// Package streamlog correlates stream attempts without logging playback secrets.
package streamlog

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

var ErrWinnerSelected = errors.New("another stream attempt won")

var sequence atomic.Uint64

type contextKey struct{}

type trace struct {
	logger           *log.Logger
	id, provider     string
	mediaID, episode int
	translation      string
	started          time.Time
}

func WithResolution(ctx context.Context, logger *log.Logger, mediaID, episode int, translation string) context.Context {
	return context.WithValue(ctx, contextKey{}, trace{
		logger: logger, id: fmt.Sprintf("r%d-%d", os.Getpid(), sequence.Add(1)),
		mediaID: mediaID, episode: episode, translation: translation,
		started: time.Now(),
	})
}

func WithProvider(ctx context.Context, name string) context.Context {
	value, ok := ctx.Value(contextKey{}).(trace)
	if !ok {
		return ctx
	}
	value.provider = name
	return context.WithValue(ctx, contextKey{}, value)
}

// Start returns an idempotent terminal logger. Cancellation also completes an
// attempt, even if its implementation is slow to return. Arguments must be
// non-secret identifiers/reason codes, never URLs, headers, or raw error text.
func Start(ctx context.Context, source, stage string) func(status, reason string) {
	value, ok := ctx.Value(contextKey{}).(trace)
	if !ok || value.logger == nil {
		return func(string, string) {}
	}
	started := time.Now()
	value.write("stream_attempt", source, stage, "started", "", 0)
	var once sync.Once
	finish := func(status, reason string) {
		once.Do(func() {
			if ctx.Err() != nil {
				status, reason = Outcome(ctx, ctx.Err())
			}
			value.write("stream_attempt", source, stage, status, reason, time.Since(started))
		})
	}
	stop := context.AfterFunc(ctx, func() {
		status, reason := Outcome(ctx, ctx.Err())
		finish(status, reason)
	})
	return func(status, reason string) {
		finish(status, reason)
		stop()
	}
}

// Outcome classifies errors without exposing arbitrary upstream error text.
func Outcome(ctx context.Context, err error) (status, reason string) {
	if errors.Is(context.Cause(ctx), ErrWinnerSelected) {
		return "cancelled", "winner_selected"
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return "timeout", "deadline_exceeded"
	}
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return "cancelled", "request_cancelled"
	}
	if err != nil {
		return "failed", "resolution_failed"
	}
	return "resolved", ""
}

func Selected(ctx context.Context, source string) {
	if value, ok := ctx.Value(contextKey{}).(trace); ok && value.logger != nil {
		value.write("stream_selected", source, "select", "resolved", "", time.Since(value.started))
	}
}

func (t trace) write(event, source, stage, status, reason string, elapsed time.Duration) {
	t.logger.Printf("%s resolution=%s media_id=%d episode=%d translation=%s provider=%s source=%s stage=%s status=%s reason=%s elapsed_ms=%.3f",
		event, t.id, t.mediaID, t.episode, identifier(t.translation), identifier(t.provider),
		identifier(source), identifier(stage), identifier(status), identifier(reason), float64(elapsed)/float64(time.Millisecond))
}

func identifier(value string) string {
	if value == "" {
		return "-"
	}
	if len(value) > 64 {
		return "redacted"
	}
	for _, char := range value {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '_' || char == '-' || char == '.') {
			return "redacted"
		}
	}
	return value
}
