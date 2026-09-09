//go:build live

package browser

import (
	"context"
	"errors"
	"log"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Live coverage for the browser resolver. It drives a real Chromium against
// the real origin, so it runs only under `make test-live`.
//
// Repeatable procedure:
//
//  1. The origin protects its watch route with an interactive bot check. Pass
//     it once by hand, in the same profile the resolver uses:
//     chromium --user-data-dir="$XDG_STATE_HOME/tarragon/anime/browser" \
//     https://mkissa.to/anime/ReHMC7TQnch3C6z8j/p-1-sub
//     Tick "Verify you are human" and wait for the player to appear. The
//     clearance cookie is stored in the profile and lasts about a year, but
//     the origin can retire it sooner, which surfaces here as ErrChallenged.
//  2. Run `make test-live`. Set ANIME_LIVE_PROFILE to point at a different
//     profile directory, and ANIME_LIVE_HEADFUL=1 to watch it work.
//
// The test reports cold resolution (browser start included) and warm
// resolution (browser already running) so regressions in either are visible.

func liveOptions(t *testing.T) Options {
	t.Helper()
	binary, err := FindBinary()
	if err != nil {
		t.Skipf("no Chromium available: %v", err)
	}
	profile := os.Getenv("ANIME_LIVE_PROFILE")
	if profile == "" {
		profile, err = DefaultProfileDir()
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(profile); err != nil {
		t.Skipf("browser profile %s is not prepared; pass the origin's bot check once by hand", profile)
	}
	return Options{
		Binary:      binary,
		ProfileDir:  profile,
		Headless:    os.Getenv("ANIME_LIVE_HEADFUL") == "",
		Timeout:     60 * time.Second,
		IdleTimeout: time.Minute,
		MaxSessions: 2,
		Logger:      log.New(os.Stderr, "browser: ", 0),
	}
}

func acceptEpisodeMedia(candidate Candidate) bool {
	if candidate.Kind != "Media" {
		return false
	}
	return strings.HasPrefix(candidate.URL, "http")
}

func TestLiveCaptureEpisodeMedia(t *testing.T) {
	resolver := New(liveOptions(t))
	defer resolver.Close()

	const page = "https://mkissa.to/anime/ReHMC7TQnch3C6z8j/p-1-sub"

	cold := time.Now()
	candidates, err := resolver.Capture(t.Context(), Request{PageURL: page, Accept: acceptEpisodeMedia})
	coldDuration := time.Since(cold)
	if errors.Is(err, ErrChallenged) {
		t.Skipf("origin served a bot challenge; pass it once by hand in %s", resolver.opts.ProfileDir)
	}
	if err != nil {
		t.Fatalf("Capture() error = %v", err)
	}
	if len(candidates) == 0 {
		t.Fatal("Capture() returned no media")
	}
	media := candidates[0]
	if media.Headers["Referer"] == "" {
		t.Fatalf("captured media has no Referer, playback outside the browser would fail: %#v", media)
	}

	warm := time.Now()
	if _, err := resolver.Capture(t.Context(), Request{PageURL: page, Accept: acceptEpisodeMedia}); err != nil {
		t.Fatalf("warm Capture() error = %v", err)
	}
	warmDuration := time.Since(warm)
	t.Logf("resolution timings: cold=%s warm=%s url=%s", coldDuration.Round(time.Millisecond), warmDuration.Round(time.Millisecond), media.URL)

	// The captured context must be enough on its own: the whole point is that
	// mpv, which has no access to the browser session, can play this.
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe is not installed; skipping the out-of-browser playback check")
	}
	probe(t, media)
}

func probe(t *testing.T, media Candidate) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()

	args := []string{"-v", "error", "-show_entries", "stream=codec_type", "-of", "csv=p=0"}
	var headers []string
	for key, value := range media.Headers {
		switch key {
		case "Referer", "User-Agent", "Origin", "Cookie":
			headers = append(headers, key+": "+value+"\r\n")
		}
	}
	if len(headers) > 0 {
		args = append(args, "-headers", strings.Join(headers, ""))
	}
	args = append(args, media.URL)

	out, err := exec.CommandContext(ctx, "ffprobe", args...).Output()
	if err != nil {
		t.Fatalf("ffprobe on the captured URL failed, the playback context is not portable: %v", err)
	}
	report := string(out)
	if !strings.Contains(report, "video") || !strings.Contains(report, "audio") {
		t.Fatalf("captured URL has no audio and video streams: %q", report)
	}
}

func TestLiveCaptureIsCancellable(t *testing.T) {
	resolver := New(liveOptions(t))
	defer resolver.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := resolver.Capture(ctx, Request{
		PageURL: "https://mkissa.to/anime/ReHMC7TQnch3C6z8j/p-1-sub",
		Accept:  func(Candidate) bool { return false },
	})
	if err == nil {
		t.Fatal("Capture() error = nil, want the deadline to be honoured")
	}
	// Chromium start-up dominates a cold run; the check is that cancellation
	// is not simply ignored until the resolver's own timeout.
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("cancelled capture took %s, want it to return promptly", elapsed)
	}
}
