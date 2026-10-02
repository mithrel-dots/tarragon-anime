//go:build live

package anipub

import (
	"context"
	"log"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"tarragon-anime/internal/browser"
	"tarragon-anime/internal/provider/providertest"
)

// Metadata resolution uses the site's own HTTP endpoints.
func TestLiveMatchAndEpisodes(t *testing.T) {
	client := NewClient(&http.Client{Timeout: 20 * time.Second})
	defer client.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	// The aliases a tracker supplies, in the order the service passes them.
	aliases := []string{
		"That Time I Got Reincarnated as a Slime Season 4",
		"Tensei Shitara Slime Datta Ken 4th Season",
	}
	anime, err := client.Match(ctx, 182205, aliases, "sub")
	if err != nil {
		t.Fatalf("Match() error = %v", err)
	}
	t.Logf("matched id=%s name=%q", anime.ID, anime.Name)

	episodes, err := client.Episodes(ctx, anime, "sub")
	if err != nil {
		t.Fatalf("Episodes() error = %v", err)
	}
	if len(episodes) == 0 {
		t.Fatal("Episodes() returned nothing")
	}
	// Episode one is stored beside the list rather than inside it, so its
	// absence is the failure this guards against.
	if episodes[0].Number != 1 {
		t.Fatalf("episodes[0].Number = %d, want the first episode", episodes[0].Number)
	}
	t.Logf("episodes=%d first=%+v last=%+v", len(episodes), episodes[0], episodes[len(episodes)-1])

	if got := watchURL(client.base, episodes[0].ShowID, episodes[0].Value, "sub"); got == "" {
		t.Fatal("watchURL() produced nothing for the first episode")
	}
}

func TestLiveBrowserStreams(t *testing.T) {
	binary, err := browser.FindBinary()
	if err != nil {
		t.Skip(err)
	}
	resolver := browser.New(browser.Options{
		Binary: binary, ProfileDir: t.TempDir(), Headless: true,
		Timeout: 60 * time.Second, Logger: log.New(os.Stderr, "browser: ", 0),
	})
	defer resolver.Close()
	client := NewClient(&http.Client{Timeout: 20 * time.Second})
	defer client.Close()
	client.UseBrowser(resolver)
	for _, mode := range []string{"sub", "dub"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()
			show, err := client.Match(ctx, 182205, []string{"That Time I Got Reincarnated as a Slime Season 4"}, mode)
			if err != nil {
				t.Fatal(err)
			}
			episodes, err := client.Episodes(ctx, show, mode)
			if err != nil || len(episodes) == 0 {
				t.Fatalf("episodes=%d error=%v", len(episodes), err)
			}
			streams, err := client.browserStreams(ctx, episodes[0], mode, "worst")
			if err != nil || len(streams) == 0 {
				t.Fatalf("streams=%d error=%v", len(streams), err)
			}
			stream, err := client.prepareStream(ctx, streams[0], "worst", nil)
			if err != nil {
				t.Fatal(err)
			}
			providertest.CheckMedia(t, ctx, stream)
		})
	}
}

func TestLiveNativeStreams(t *testing.T) {
	client := NewClient(&http.Client{Timeout: 20 * time.Second})
	defer client.Close()
	for _, title := range []struct {
		id   int
		name string
	}{
		{182205, "That Time I Got Reincarnated as a Slime Season 4"},
		{154587, "Frieren: Beyond Journey's End"},
		{1, "Cowboy Bebop"},
	} {
		for _, mode := range []string{"sub", "dub"} {
			t.Run(title.name+"/"+mode, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
				defer cancel()
				show, err := client.Match(ctx, title.id, []string{title.name}, mode)
				if err != nil {
					t.Fatal(err)
				}
				episodes, err := client.Episodes(ctx, show, mode)
				if err != nil || len(episodes) == 0 {
					t.Fatalf("episodes=%d error=%v", len(episodes), err)
				}
				start := time.Now()
				streams, err := client.Streams(ctx, episodes[0], mode, "worst")
				if err != nil || len(streams) == 0 {
					t.Fatalf("streams=%d error=%v", len(streams), err)
				}
				t.Logf("resolved via %s in %s", streams[0].Source, time.Since(start).Round(time.Millisecond))
				providertest.CheckMedia(t, ctx, streams[0])
			})
		}
	}
}

func TestLiveReZeroSeason4Finale(t *testing.T) {
	client := NewClient(&http.Client{Timeout: 20 * time.Second})
	defer client.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	show, err := client.Match(ctx, 189046, []string{"Re:ZERO -Starting Life in Another World- Season 4"}, "sub")
	if err != nil {
		t.Fatal(err)
	}
	episode, err := client.LookupEpisode(ctx, show, 19, "sub")
	if err != nil {
		t.Fatal(err)
	}
	if episode.ShowID != "61316" || episode.Number != 19 || episode.Value != "19" {
		t.Fatalf("incorrect finale route: %+v", episode)
	}
	start := time.Now()
	streams, err := client.Streams(ctx, episode, "sub", "best")
	if err != nil || len(streams) == 0 {
		t.Fatalf("streams=%d error=%v", len(streams), err)
	}
	if streams[0].Source != "native" || streams[0].Subtitle == "" {
		t.Fatal("expected native resolution with full English subtitles")
	}
	subtitle, _, err := client.resource(ctx, streams[0].Subtitle, streams[0].Headers)
	if err != nil || !strings.HasPrefix(strings.TrimSpace(string(subtitle)), "WEBVTT") || !strings.Contains(string(subtitle), "-->") {
		t.Fatalf("English subtitle track is not readable WebVTT: %v", err)
	}
	t.Logf("resolved via %s in %s", streams[0].Source, time.Since(start).Round(time.Millisecond))
	providertest.CheckMedia(t, ctx, streams[0])
}
