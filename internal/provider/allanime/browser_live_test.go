//go:build live

package allanime

import (
	"context"
	"errors"
	"log"
	"os"
	"testing"
	"time"

	"tarragon-anime/internal/browser"
)

// liveResolver builds the managed resolver the daemon would build. See
// internal/browser/live_test.go for the one-off manual step the origin's bot
// check requires.
func liveResolver(t *testing.T) *browser.Resolver {
	t.Helper()
	binary, err := browser.FindBinary()
	if err != nil {
		t.Skipf("no Chromium available: %v", err)
	}
	profile := os.Getenv("ANIME_LIVE_PROFILE")
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
		Timeout:     60 * time.Second,
		IdleTimeout: time.Minute,
		MaxSessions: 2,
		Logger:      log.New(os.Stderr, "browser: ", 0),
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
			if errors.Is(err, browser.ErrChallenged) {
				t.Skip("origin served a bot challenge; pass it once by hand")
			}
			if err != nil {
				t.Fatalf("Streams() error = %v", err)
			}
			if len(streams) == 0 || streams[0].URL == "" {
				t.Fatalf("Streams() = %#v, want a playable URL", streams)
			}
			if streams[0].Headers["Referer"] == "" {
				t.Fatalf("stream has no Referer, mpv would be refused: %#v", streams[0])
			}
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
		if errors.Is(err, browser.ErrChallenged) {
			t.Skip("origin served a bot challenge; pass it once by hand")
		}
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
