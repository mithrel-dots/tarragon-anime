//go:build live

package browser

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-rod/rod/lib/proto"
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
	after, err := (proto.TargetGetTargets{}).Call(b)
	if err != nil {
		t.Fatal(err)
	}
	// Chromium can create its own startup targets after Connect returns.
	for _, target := range after.TargetInfos {
		if target.URL == req.PageURL {
			t.Fatalf("cancelled capture leaked target %s", target.TargetID)
		}
	}
}
