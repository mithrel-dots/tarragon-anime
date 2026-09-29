//go:build live

package anime

import (
	"context"
	"log"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"

	"tarragon-anime/internal/anilist"
	browserpkg "tarragon-anime/internal/browser"
	"tarragon-anime/internal/mpv"
	"tarragon-anime/internal/provider"
	"tarragon-anime/internal/provider/allanime"
	"tarragon-anime/internal/provider/animeflix"
	"tarragon-anime/internal/provider/animepahe"
	"tarragon-anime/internal/provider/anineko"
	"tarragon-anime/internal/provider/anipub"
)

// This exercises the configured provider race without writing watch progress.
func TestLiveProviderRace(t *testing.T) {
	number := 18
	if value := os.Getenv("ANIME_LIVE_EPISODE"); value != "" {
		var err error
		number, err = strconv.Atoi(value)
		if err != nil || number <= 0 {
			t.Fatal("ANIME_LIVE_EPISODE must be a positive integer")
		}
	}
	path, err := ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	profile := os.Getenv("ANIME_LIVE_PROFILE")
	if profile == "" {
		profile = config.Browser.ProfileDir
	}
	if os.Getenv("ANIME_LIVE_FRESH") != "" {
		profile = t.TempDir()
	}
	if profile == "" {
		profile, err = browserpkg.DefaultProfileDir()
		if err != nil {
			t.Fatal(err)
		}
	}
	logger := log.New(os.Stderr, "race: ", 0)
	resolver := browserpkg.New(browserpkg.Options{
		Binary: config.Browser.Binary, ProfileDir: profile,
		Headless: os.Getenv("ANIME_LIVE_HEADFUL") == "", Timeout: config.Browser.Timeout,
		MaxSessions: config.Browser.MaxSessions, AutoClearance: config.Browser.AutoClearance,
		ClearanceWait: config.Browser.ClearanceWait, ClearanceTimeout: config.Browser.ClearanceTimeout,
		Logger: logger,
	})
	defer resolver.Close()
	httpClient := &http.Client{Timeout: 20 * time.Second}
	all := allanime.NewClient(httpClient)
	pub := anipub.NewClient(httpClient)
	defer pub.Close()
	if config.Browser.Enabled {
		all.UseBrowser(resolver)
		pub.UseBrowser(resolver)
	}
	flix, neko := animeflix.NewClient(httpClient), anineko.NewClient(httpClient)
	defer flix.Close()
	defer neko.Close()
	service := NewServiceWithProviders(anilist.NewClient(httpClient), map[string]provider.Client{
		"animeflix": flix, "anineko": neko, "animepahe": animepahe.NewClient(httpClient).WithBrowser(config.Browser.Enabled),
		"allanime": all, "anipub": pub,
	}, nil, nil, nil, nil, config, logger)
	defer service.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	start := time.Now()
	stream, episode, err := service.resolveStream(ctx, Episode{MediaID: 182205, Number: number})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("provider=%s resolved episode=%d in %s", episode.Provider, episode.Number, time.Since(start).Round(time.Millisecond))
	if os.Getenv("ANIME_LIVE_MEDIA") != "1" {
		return
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	player := mpv.New([]string{"--vo=null", "--ao=null", "--frames=1", "--load-scripts=no", "--ytdl=no", "--network-timeout=10"}, logger)
	probeCtx, stop := context.WithTimeout(ctx, 30*time.Second)
	defer stop()
	session, err := player.Play(probeCtx, stream, episode.Title, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	loaded := false
	for {
		select {
		case event, ok := <-session.Events():
			if !ok {
				t.Fatal("mpv closed before decoding a frame")
			}
			if event.Type == mpv.EventFileLoaded {
				loaded = true
			}
			if event.Type == mpv.EventEndFile {
				if !loaded || event.Reason != "eof" {
					t.Fatalf("winner failed playback: loaded=%t reason=%s error=%s", loaded, event.Reason, event.Error)
				}
				t.Log("winning stream decoded a frame after losing lookups were cancelled")
				return
			}
		case <-probeCtx.Done():
			t.Fatal(probeCtx.Err())
		}
	}
}
