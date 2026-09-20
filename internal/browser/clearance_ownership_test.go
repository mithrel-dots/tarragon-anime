package browser

import (
	"context"
	"errors"
	"testing"
	"time"

	"tarragon-anime/internal/streamlog"
)

func waitClearanceOwners(t *testing.T, r *Resolver, count int) *clearanceSession {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		session := r.clearing
		ok := session != nil && len(session.owners) == count
		r.mu.Unlock()
		if ok {
			return session
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("clearance owners did not register")
	return nil
}

func awaitClearanceClose(t *testing.T, session *clearanceSession) {
	t.Helper()
	select {
	case <-session.done:
	case <-time.After(time.Second):
		t.Fatal("abandoned clearance retained the browser profile")
	}
}

func TestClearanceLoserClosesLateWindow(t *testing.T) {
	withDisplay(t)
	started := make(chan struct{})
	window := &fakeClearance{}
	r := newTestResolver(t, clearanceOptions(), func(context.Context, Options) (engine, error) { return &fakeEngine{}, nil })
	r.newClearance = func(ctx context.Context, _ Options, _ string) (clearance, error) {
		close(started)
		<-ctx.Done()
		return window, nil
	}
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	result := make(chan error, 1)
	go func() { result <- r.Clear(ctx, testRequest().PageURL) }()
	<-started
	session := waitClearanceOwners(t, r, 1)
	cancel(streamlog.ErrWinnerSelected)
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	awaitClearanceClose(t, session)
	if window.closes() != 1 {
		t.Fatal("late loser window not closed exactly once")
	}
}

func TestClearanceLoserDoesNotCloseAnotherOwnersWindow(t *testing.T) {
	withDisplay(t)
	window := &fakeClearance{}
	opts := clearanceOptions()
	opts.ClearanceWait = 3 * time.Second
	r := newTestResolver(t, opts, func(context.Context, Options) (engine, error) { return &fakeEngine{}, nil })
	opened := make(chan struct{})
	r.newClearance = func(context.Context, Options, string) (clearance, error) { close(opened); return window, nil }
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	loser, other := make(chan error, 1), make(chan error, 1)
	go func() { loser <- r.Clear(ctx, testRequest().PageURL) }()
	<-opened
	go func() { other <- r.Clear(t.Context(), testRequest().PageURL) }()
	session := waitClearanceOwners(t, r, 2)
	cancel(streamlog.ErrWinnerSelected)
	if err := <-loser; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	r.mu.Lock()
	abandoned, owners := session.abandoned, len(session.owners)
	r.mu.Unlock()
	if abandoned || owners != 1 || window.closes() != 0 {
		t.Fatal("loser closed a shared window")
	}
	window.pass()
	if err := <-other; err != nil {
		t.Fatal(err)
	}
	if window.closes() != 1 {
		t.Fatal("shared clearance did not finish normally")
	}
}

func TestPendingClearanceRetainsHumanButNotWinnerLoser(t *testing.T) {
	withDisplay(t)
	for _, winner := range []bool{false, true} {
		t.Run(map[bool]string{false: "human", true: "loser"}[winner], func(t *testing.T) {
			opts := clearanceOptions()
			opts.ClearanceWait = 20 * time.Millisecond
			window := &fakeClearance{}
			r := newTestResolver(t, opts, func(context.Context, Options) (engine, error) { return &fakeEngine{err: ErrChallenged}, nil })
			r.newClearance = func(context.Context, Options, string) (clearance, error) { return window, nil }
			parent, cleanup := context.WithCancel(t.Context())
			defer cleanup()
			ctx, cancel := context.WithCancelCause(parent)
			defer cancel(nil)
			if _, err := r.Capture(ctx, testRequest()); !errors.Is(err, ErrClearancePending) {
				t.Fatal(err)
			}
			r.mu.Lock()
			session := r.clearing
			pending := session != nil && len(session.owners) == 1
			if session != nil {
				for owner := range session.owners {
					pending = pending && owner.pending
				}
			}
			r.mu.Unlock()
			if !pending {
				t.Fatal("Capture returned pending before committing owner handoff")
			}
			if winner {
				cancel(streamlog.ErrWinnerSelected)
			}
			// Core cancels losers first, then releases all completed lookup contexts.
			cleanup()
			if !winner {
				select {
				case <-session.done:
					t.Fatal("request cleanup destroyed the manual pending workflow")
				case <-time.After(30 * time.Millisecond):
				}
				window.pass()
			}
			awaitClearanceClose(t, session)
			if window.closes() != 1 {
				t.Fatal("pending window was not closed once")
			}
		})
	}
}

func TestClearanceLoserDoesNotStopWinningCapture(t *testing.T) {
	withDisplay(t)
	active, release := make(chan struct{}), make(chan struct{})
	eng := &fakeEngine{block: release, hook: func() error { close(active); return nil }}
	r := newTestResolver(t, clearanceOptions(), func(context.Context, Options) (engine, error) { return eng, nil })
	r.newClearance = func(context.Context, Options, string) (clearance, error) {
		t.Error("abandoned clearance opened an unnecessary window")
		return &fakeClearance{}, nil
	}
	winner := make(chan error, 1)
	go func() { _, err := r.Capture(t.Context(), testRequest()); winner <- err }()
	<-active
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	loser := make(chan error, 1)
	go func() { loser <- r.Clear(ctx, testRequest().PageURL) }()
	session := waitClearanceOwners(t, r, 1)
	cancel(streamlog.ErrWinnerSelected)
	if err := <-loser; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	awaitClearanceClose(t, session)
	close(release)
	if err := <-winner; err != nil {
		t.Fatal(err)
	}
	if _, closed := eng.counts(); closed != 0 {
		t.Fatal("clearance stopped the winning browser")
	}
}
