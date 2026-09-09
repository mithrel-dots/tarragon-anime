//go:build live

package browser

import (
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

// This uses real Chromium and a deterministic local challenge, not a live
// provider. It proves the cookie survives both clearance and resolver restarts.
func TestLiveClearancePersistsAcrossRestarts(t *testing.T) {
	if os.Getenv("ANIME_LIVE_CLEARANCE") == "" || !hasDisplay() {
		t.Skip("set ANIME_LIVE_CLEARANCE=1 with a display to open real Chromium")
	}
	bin, err := FindBinary()
	if err != nil {
		t.Fatal(err)
	}
	var visits, authenticated atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/watch":
			n := visits.Add(1)
			if _, err := r.Cookie("cf_clearance"); err == nil {
				authenticated.Add(1)
			} else if n == 2 {
				http.SetCookie(w, &http.Cookie{Name: "cf_clearance", Value: "local-test", Path: "/", MaxAge: 3600, HttpOnly: true})
			} else {
				http.Error(w, "test bot check", http.StatusForbidden)
				return
			}
			http.Redirect(w, r, "/player", http.StatusFound)
		case "/player":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<title>Anime browser clearance test</title><p>Local test check passed. This window closes automatically.</p><script>fetch('/episode.mp4')</script>`)
		case "/episode.mp4":
			w.Header().Set("Content-Type", "video/mp4")
			fmt.Fprint(w, "test network capture")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	opts := Options{
		Binary: bin, ProfileDir: t.TempDir(), Headless: true, AutoClearance: true,
		Timeout: 60 * time.Second, ClearanceWait: 30 * time.Second, ClearanceTimeout: 45 * time.Second,
		Logger: log.New(os.Stderr, "browser: ", 0),
	}
	req := Request{PageURL: server.URL + "/watch", Accept: func(c Candidate) bool { return c.URL == server.URL+"/episode.mp4" }}
	for i := range 2 {
		resolver := New(opts)
		t.Cleanup(resolver.Close)
		found, err := resolver.Capture(t.Context(), req)
		resolver.Close()
		if err != nil || len(found) != 1 {
			t.Fatalf("capture %d = %d candidates, %v", i+1, len(found), err)
		}
	}
	if visits.Load() != 4 || authenticated.Load() != 2 {
		t.Fatalf("watch visits=%d authenticated=%d, want 4 and 2 (retry and restart)", visits.Load(), authenticated.Load())
	}
}

// TestLiveClearanceOpensAWindow drives the real challenge workflow against a
// fresh profile, which is the only way to be sure a window actually appears
// and that the clearance ends up in the profile.
//
//	ANIME_LIVE_CLEARANCE=1 go test -tags live ./internal/browser -run TestLiveClearance -v
func TestLiveClearanceOpensAWindow(t *testing.T) {
	if os.Getenv("ANIME_LIVE_CLEARANCE") == "" {
		t.Skip("set ANIME_LIVE_CLEARANCE=1 to open a real browser window")
	}
	bin, err := FindBinary()
	if err != nil {
		t.Skip(err)
	}
	if !hasDisplay() {
		t.Skip("no display available")
	}
	page := os.Getenv("ANIME_LIVE_PAGE")
	if page == "" {
		t.Skip("set ANIME_LIVE_PAGE to the challenged page")
	}

	profile := os.Getenv("ANIME_LIVE_KEEP")
	if profile == "" {
		profile = t.TempDir()
	}
	resolver := New(Options{
		Binary: bin, ProfileDir: profile, Headless: true,
		AutoClearance: true, ClearanceWait: 45 * time.Second, ClearanceTimeout: 2 * time.Minute,
		Logger: log.New(os.Stderr, "browser: ", 0),
	})
	t.Cleanup(resolver.Close)

	start := time.Now()
	if err := resolver.Clear(t.Context(), page); err != nil {
		t.Fatalf("Clear() error = %v", err)
	}
	t.Logf("clearance earned in %s", time.Since(start).Round(time.Millisecond))
	if resolver.clearancePending() {
		t.Fatal("clearance window was left open")
	}
}
