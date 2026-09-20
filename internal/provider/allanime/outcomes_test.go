package allanime

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"sync/atomic"
	"testing"

	"tarragon-anime/internal/browser"
	"tarragon-anime/internal/streamlog"
)

func TestStreamsValidationOutcomesAndSafeSources(t *testing.T) {
	for _, test := range []struct {
		name, source, wantSource, reason string
		mutate                           func(*browser.Candidate)
		wantErr                          bool
	}{
		{name: "resolved", source: "source-2", wantSource: "source-2", reason: "validated_media"},
		{name: "unsafe-source", source: "private-token", wantSource: "default", reason: "validated_media"},
		{name: "credentials", wantSource: "default", reason: "credentials_required", wantErr: true, mutate: func(c *browser.Candidate) { c.Headers["cOoKiE"] = "private-token" }},
		{name: "status", wantSource: "default", reason: "no_playable_media", wantErr: true, mutate: func(c *browser.Candidate) { c.Status = 403 }},
		{name: "identity", wantSource: "default", reason: "no_playable_media", wantErr: true, mutate: func(c *browser.Candidate) {
			c.URL = "https://cdn.test/videos/wrong-show/dub/99.mp4?token=private-token"
		}},
		{name: "tracks", wantSource: "default", reason: "unsupported_tracks", wantErr: true, mutate: func(c *browser.Candidate) {
			c.URL = "https://cdn.test/opaque.m4s?token=private-token"
			c.Bytes = 1 << 20
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var logs bytes.Buffer
			ctx := streamlog.WithResolution(t.Context(), log.New(&logs, "", 0), 42, 1, "sub")
			c := NewClient(nil)
			candidate := playedCandidate()
			candidate.URL += "?token=private-token"
			candidate.Source = test.source
			candidate.Headers = map[string]string{"Referer": "https://player.test/?token=private-token"}
			if test.mutate != nil {
				test.mutate(&candidate)
			}
			c.UseBrowser(captureFunc(func(context.Context, browser.Request) ([]browser.Candidate, error) {
				return []browser.Candidate{candidate}, nil
			}))
			streams, err := c.Streams(ctx, testEpisode(), "sub", "best")
			if (err != nil) != test.wantErr {
				t.Fatalf("Streams error=%v", err)
			}
			status := "resolved"
			if test.wantErr {
				status = "unavailable"
			}
			want := "provider=allanime source=" + test.wantSource + " stage=validation status=" + status + " reason=" + test.reason
			if !strings.Contains(logs.String(), want) {
				t.Fatalf("missing validation outcome %s\n%s", want, logs.String())
			}
			if !test.wantErr {
				if len(streams) != 1 || streams[0].Source != test.wantSource {
					t.Fatalf("source provenance: %#v", streams)
				}
				cached, err := c.Streams(ctx, testEpisode(), "sub", "best")
				if err != nil || len(cached) != 1 || cached[0].Source != test.wantSource {
					t.Fatalf("cache provenance lost: %#v %v", cached, err)
				}
				if !strings.Contains(logs.String(), "source="+test.wantSource+" stage=cache status=resolved reason=validated_cache") {
					t.Fatal("missing cache outcome")
				}
			}
			for _, secret := range []string{"private-token", "https://", "Cookie", "Referer", "stream_selected", "status=playing"} {
				if strings.Contains(logs.String(), secret) {
					t.Fatalf("outcomes contain forbidden data: %q", secret)
				}
			}
		})
	}
}

func TestCancelledStreamsNeverPublishOrInvalidateSharedCache(t *testing.T) {
	for _, withWinner := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty-cache", true: "shared-winner"}[withWinner], func(t *testing.T) {
			c := NewClient(nil)
			entered, release := make(chan struct{}), make(chan struct{})
			var calls atomic.Int32
			c.UseBrowser(captureFunc(func(context.Context, browser.Request) ([]browser.Candidate, error) {
				candidate := playedCandidate()
				candidate.Source = "source-2"
				if calls.Add(1) == 1 {
					close(entered)
					<-release
					candidate.Source = "source-1"
					candidate.URL += "?token=loser-secret"
				}
				return []browser.Candidate{candidate}, nil
			}))
			var logs bytes.Buffer
			ctx, cancel := context.WithCancelCause(streamlog.WithResolution(t.Context(), log.New(&logs, "", 0), 42, 1, "sub"))
			defer cancel(nil)
			result := make(chan error, 1)
			go func() {
				streams, err := c.Streams(ctx, testEpisode(), "sub", "best")
				if len(streams) != 0 {
					t.Error("cancelled result returned streams")
				}
				result <- err
			}()
			<-entered
			if withWinner {
				if _, err := c.Streams(t.Context(), testEpisode(), "sub", "best"); err != nil {
					t.Fatal(err)
				}
			}
			cancel(streamlog.ErrWinnerSelected)
			close(release)
			if err := <-result; !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			cached, ok := c.cachedStreams(testEpisode(), "sub", "best")
			if ok != withWinner || ok && (len(cached) != 1 || cached[0].Source != "source-2") {
				t.Fatalf("cancelled worker changed cache: %#v", cached)
			}
			if !strings.Contains(logs.String(), "stage=streams status=cancelled reason=winner_selected") || strings.Contains(logs.String(), "loser-secret") {
				t.Fatalf("bad cancellation outcome: %s", logs.String())
			}
			if _, err := c.Streams(ctx, testEpisode(), "sub", "best"); !errors.Is(err, context.Canceled) {
				t.Fatal("cancelled request consumed a cache hit")
			}
		})
	}
}

func TestCacheCancellationUnderPublicationLock(t *testing.T) {
	c := NewClient(nil)
	winner := []Stream{{URL: "winner", Source: "source-2"}}
	if err := c.cacheStreams(t.Context(), testEpisode(), "sub", "best", winner); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	c.cacheMu.Lock()
	done := make(chan error, 1)
	go func() { done <- c.cacheStreams(ctx, testEpisode(), "sub", "best", []Stream{{URL: "loser"}}) }()
	cancel(streamlog.ErrWinnerSelected)
	c.cacheMu.Unlock()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	streams, ok := c.cachedStreams(testEpisode(), "sub", "best")
	if !ok || streams[0].URL != "winner" {
		t.Fatal("loser replaced shared cache")
	}
}
