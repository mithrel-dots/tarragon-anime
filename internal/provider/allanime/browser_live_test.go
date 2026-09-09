//go:build live

package allanime

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"tarragon-anime/internal/browser"
)

// liveResolver matches the daemon's automatic clearance workflow. Set
// ANIME_LIVE_FRESH=1 to require clearance in a fresh, temporary profile.
func liveResolver(t *testing.T) *browser.Resolver {
	t.Helper()
	binary, err := browser.FindBinary()
	if err != nil {
		t.Skipf("no Chromium available: %v", err)
	}
	profile := os.Getenv("ANIME_LIVE_PROFILE")
	if os.Getenv("ANIME_LIVE_FRESH") != "" {
		profile = t.TempDir()
	}
	if profile == "" {
		profile, err = browser.DefaultProfileDir()
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(profile); err != nil {
		t.Skipf("browser profile %s is not prepared; pass the origin's bot check once by hand", profile)
	}
	resolver := browser.New(browser.Options{
		Binary:      binary,
		ProfileDir:  profile,
		Headless:    os.Getenv("ANIME_LIVE_HEADFUL") == "",
		Timeout:     90 * time.Second,
		IdleTimeout: time.Minute,
		MaxSessions: 2,
		// Match the daemon: a challenged capture opens a window and retries.
		AutoClearance:    true,
		ClearanceWait:    45 * time.Second,
		ClearanceTimeout: 2 * time.Minute,
		Logger:           log.New(os.Stderr, "browser: ", 0),
	})
	t.Cleanup(resolver.Close)
	return resolver
}

// TestLiveBrowserStreamsAcrossTitles resolves several unrelated titles through
// one browser so upstream rotation and per-title player differences show up.
func TestLiveBrowserStreamsAcrossTitles(t *testing.T) {
	resolver := liveResolver(t)
	client := NewClient(nil).WithLogger(log.New(os.Stderr, "allanime: ", 0))
	client.UseBrowser(resolver)

	titles := []struct {
		name      string
		aniListID int
		aliases   []string
	}{
		{"Frieren", 154587, []string{"Sousou no Frieren", "Frieren: Beyond Journey's End"}},
		{"Steins;Gate", 9253, []string{"Steins;Gate"}},
		{"Cowboy Bebop", 1, []string{"Cowboy Bebop"}},
	}
	for _, title := range titles {
		t.Run(title.name, func(t *testing.T) {
			ctx, cancel := contextWithTimeout(t, 120*time.Second)
			defer cancel()

			show, err := client.Match(ctx, title.aniListID, title.aliases, "sub")
			if err != nil {
				t.Skipf("no conservative provider match: %v", err)
			}
			episodes, err := client.Episodes(ctx, show, "sub")
			if err != nil || len(episodes) == 0 {
				t.Fatalf("Episodes() = %d episodes, error = %v", len(episodes), err)
			}

			// Resolve uncached so the browser really runs for every title.
			client.InvalidateStreams(episodes[0], "sub", "best")
			start := time.Now()
			streams, err := client.Streams(ctx, episodes[0], "sub", "best")
			if err != nil {
				t.Fatalf("Streams() error = %v", err)
			}
			if len(streams) == 0 || streams[0].URL == "" {
				t.Fatalf("Streams() = %#v, want a playable URL", streams)
			}
			if streams[0].Headers["Referer"] == "" {
				t.Fatalf("stream has no Referer, mpv would be refused: %#v", streams[0])
			}
			probeHandoff(ctx, t, streams[0])
			t.Logf("%s ep %d resolved in %s: %s", title.name, episodes[0].Number,
				time.Since(start).Round(time.Millisecond), streams[0].URL)
		})
	}
}

// TestLiveBrowserPrefetchReusesOneBrowser mirrors what next-episode prefetch
// does: resolve the current and the following episode back to back.
func TestLiveBrowserPrefetchReusesOneBrowser(t *testing.T) {
	resolver := liveResolver(t)
	client := NewClient(nil)
	client.UseBrowser(resolver)

	ctx, cancel := contextWithTimeout(t, 180*time.Second)
	defer cancel()

	show, err := client.Match(ctx, 154587, []string{"Sousou no Frieren"}, "sub")
	if err != nil {
		t.Fatal(err)
	}
	episodes, err := client.Episodes(ctx, show, "sub")
	if err != nil || len(episodes) < 2 {
		t.Fatalf("Episodes() = %d episodes, error = %v", len(episodes), err)
	}

	cold := time.Now()
	if _, err := client.Streams(ctx, episodes[0], "sub", "best"); err != nil {
		t.Fatalf("Streams() error = %v", err)
	}
	coldDuration := time.Since(cold)

	warm := time.Now()
	if _, err := client.Streams(ctx, episodes[1], "sub", "best"); err != nil {
		t.Fatalf("prefetch Streams() error = %v", err)
	}
	warmDuration := time.Since(warm)

	cached := time.Now()
	if _, err := client.Streams(ctx, episodes[0], "sub", "best"); err != nil {
		t.Fatalf("cached Streams() error = %v", err)
	}
	cachedDuration := time.Since(cached)

	t.Logf("resolution timings: cold=%s warm=%s cached=%s",
		coldDuration.Round(time.Millisecond),
		warmDuration.Round(time.Millisecond),
		cachedDuration.Round(time.Millisecond))

	if cachedDuration > time.Second {
		t.Fatalf("cached resolution took %s, want it served from memory", cachedDuration)
	}
}

func contextWithTimeout(t *testing.T, d time.Duration) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(t.Context(), d)
}

// probeHandoff replays the captured stream outside the browser with only the
// headers the resolver returned. A reachable URL is not enough: the page also
// plays covers and animated emoji through a media element, and those are valid
// media to a probe, so the result has to look like a full episode.
func probeHandoff(ctx context.Context, t *testing.T, stream Stream) {
	t.Helper()
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skipf("ffprobe is required to verify the handoff: %v", err)
	}
	// ffmpeg takes one header block; repeating -headers replaces it.
	var headers strings.Builder
	for key, value := range stream.Headers {
		fmt.Fprintf(&headers, "%s: %s\r\n", key, value)
	}
	out, err := exec.CommandContext(ctx, "ffprobe", "-v", "error",
		"-headers", headers.String(),
		"-show_entries", "format=duration:stream=codec_type",
		"-of", "default=nw=1", stream.URL).CombinedOutput()
	if err != nil {
		t.Fatalf("handoff is not playable outside the browser: %v\n%s", err, out)
	}
	report := string(out)
	if !strings.Contains(report, "codec_type=video") {
		t.Fatalf("handoff carries no video stream:\n%s", report)
	}
	var seconds float64
	for line := range strings.SplitSeq(report, "\n") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), "duration="); ok {
			seconds, _ = strconv.ParseFloat(value, 64)
		}
	}
	if seconds < minEpisodeSeconds {
		t.Fatalf("handoff duration = %.1fs, want a full episode; a decoy asset was selected:\n%s", seconds, report)
	}
	t.Logf("handoff verified: %.0fs of video", seconds)
}

// minEpisodeSeconds is below any real episode and far above the bumpers,
// previews and emoji loops the page also plays.
const minEpisodeSeconds = 600
