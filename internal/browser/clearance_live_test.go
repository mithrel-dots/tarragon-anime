//go:build live

package browser

import (
	"log"
	"os"
	"testing"
	"time"
)

// TestLiveClearanceUsesOrdinaryChromium verifies the interactive path against
// the real origin. Unlike the resolver, this browser is intentionally not
// connected through CDP: Cloudflare rejects the automated fingerprint.
//
// Set ANIME_LIVE_CLEARANCE=1 and ANIME_LIVE_PAGE, tick the checkbox, wait for
// the protected page, then close the Chromium window to finish the test.
func TestLiveClearanceUsesOrdinaryChromium(t *testing.T) {
	if os.Getenv("ANIME_LIVE_CLEARANCE") == "" {
		t.Skip("set ANIME_LIVE_CLEARANCE=1 to open a real browser window")
	}
	if !hasDisplay() {
		t.Skip("no display available")
	}
	page := os.Getenv("ANIME_LIVE_PAGE")
	if page == "" {
		t.Skip("set ANIME_LIVE_PAGE to the challenged page")
	}
	bin, err := FindBinary()
	if err != nil {
		t.Fatal(err)
	}
	profile := os.Getenv("ANIME_LIVE_KEEP")
	if profile == "" {
		profile = t.TempDir()
	}
	resolver := New(Options{
		Binary: bin, ProfileDir: profile, Headless: true, AutoClearance: true,
		ClearanceWait: 5 * time.Minute, ClearanceTimeout: 10 * time.Minute,
		Logger: log.New(os.Stderr, "browser: ", 0),
	})
	t.Cleanup(resolver.Close)
	if err := resolver.Clear(t.Context(), page); err != nil {
		t.Fatalf("Clear() error = %v", err)
	}
}
