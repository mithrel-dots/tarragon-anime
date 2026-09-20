//go:build live

package browser

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-rod/rod/lib/proto"
	"tarragon-anime/internal/streamlog"
)

func TestLiveChromeLifecycle(t *testing.T) {
	bin, err := FindBinary()
	if err != nil {
		t.Skip(err)
	}
	opts := New(Options{Binary: bin, ProfileDir: t.TempDir(), Headless: true}).opts
	startup, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	eng, err := newChromeEngine(startup, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.close()
	cancel()
	chrome := eng.(*chromeEngine)
	check, stop := context.WithTimeout(t.Context(), 5*time.Second)
	defer stop()
	b := chrome.browser.Context(check)
	if _, err := (proto.BrowserGetVersion{}).Call(b); err != nil {
		t.Fatalf("shared browser died with startup context: %v", err)
	}
	ctx, stopCapture := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer stopCapture()
	req := Request{PageURL: "data:text/html,<title>lifecycle</title>", Accept: func(Candidate) bool { return false }}
	_, err = chrome.capture(ctx, req)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("capture() = %v, want deadline exceeded", err)
	}
	// closeTarget acknowledges before destruction; ignore unrelated startup targets.
	deadline := time.Now().Add(2 * time.Second)
	for {
		after, err := (proto.TargetGetTargets{}).Call(b)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, target := range after.TargetInfos {
			found = found || target.URL == req.PageURL
		}
		if !found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cancelled capture target survived cleanup")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestLiveLocalWinnerCancellationKeepsSharedBrowser(t *testing.T) {
	bin, err := FindBinary()
	if err != nil {
		t.Skip(err)
	}
	winnerStarted, loserStarted, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var winnerOnce, loserOnce, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/winner":
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, `<!doctype html><script>fetch('/episode.mp4?token=private-token')</script>`)
		case "/loser":
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, `<!doctype html><script>fetch('/started')</script>`)
		case "/episode.mp4":
			winnerOnce.Do(func() { close(winnerStarted) })
			select {
			case <-release:
			case <-req.Context().Done():
				return
			}
			w.Header().Set("Content-Type", "video/mp4")
			w.WriteHeader(http.StatusPartialContent)
			io.WriteString(w, "local fixture")
		case "/started":
			loserOnce.Do(func() { close(loserStarted) })
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(server.Close)
	t.Cleanup(unblock)
	r := New(Options{Binary: bin, ProfileDir: t.TempDir(), Headless: true, Timeout: 15 * time.Second})
	t.Cleanup(r.Close)
	ctx, stop := context.WithTimeout(t.Context(), 20*time.Second)
	defer stop()
	var winnerLogs, loserLogs outcomeLog
	loserCtx, cancel := context.WithCancelCause(traceCapture(ctx, &loserLogs))
	defer cancel(nil)
	winner, loser := make(chan error, 1), make(chan error, 1)
	go func() {
		found, err := r.Capture(traceCapture(ctx, &winnerLogs), Request{PageURL: server.URL + "/winner", Settle: 100 * time.Millisecond, Accept: func(c Candidate) bool { return strings.Contains(c.URL, "/episode.mp4") }})
		if err == nil && (len(found) != 1 || found[0].Source != "default") {
			t.Error("winner lost its source provenance")
		}
		winner <- err
	}()
	select {
	case <-winnerStarted:
	case <-ctx.Done():
		t.Fatal("winner did not start")
	}
	go func() {
		_, err := r.Capture(loserCtx, Request{PageURL: server.URL + "/loser", Accept: func(Candidate) bool { return false }})
		loser <- err
	}()
	select {
	case <-loserStarted:
	case <-ctx.Done():
		t.Fatal("loser did not start")
	}
	r.mu.Lock()
	eng, active := r.eng.(*chromeEngine), r.active
	r.mu.Unlock()
	if active != 2 || cap(r.sem) != 2 {
		t.Fatal("captures did not share the existing two slots")
	}
	cancel(streamlog.ErrWinnerSelected)
	if err := <-loser; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	// closeTarget acknowledges the command before target destruction is visible.
	deadline := time.Now().Add(2 * time.Second)
	for {
		targets, err := (proto.TargetGetTargets{}).Call(eng.browser.Context(ctx))
		if err != nil {
			t.Fatal(err)
		}
		foundWinner, foundLoser := false, false
		for _, target := range targets.TargetInfos {
			foundLoser = foundLoser || target.URL == server.URL+"/loser"
			foundWinner = foundWinner || target.URL == server.URL+"/winner"
		}
		if !foundWinner {
			t.Fatal("loser cancellation closed the winning page")
		}
		if !foundLoser {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("loser capture page survived cancellation")
		}
		time.Sleep(10 * time.Millisecond)
	}
	unblock()
	if err := <-winner; err != nil {
		t.Fatal(err)
	}
	if _, err := (proto.BrowserGetVersion{}).Call(eng.browser.Context(ctx)); err != nil {
		t.Fatalf("shared browser stopped: %v", err)
	}
	assertSourceOutcomes(t, loserLogs.String(), map[string]string{"default": "cancelled"})
	assertSourceOutcomes(t, winnerLogs.String(), map[string]string{"default": "resolved"})
	if !strings.Contains(loserLogs.String(), "reason=winner_selected") || strings.Contains(winnerLogs.String(), "private-token") {
		t.Fatal("incorrect or unsafe outcomes")
	}
}
