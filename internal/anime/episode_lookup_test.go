package anime

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"sync/atomic"
	"testing"

	"tarragon-anime/internal/anilist"
	"tarragon-anime/internal/provider"
)

type lookupTestProvider struct {
	raceTestProvider
	lookup func(context.Context, provider.Anime, int, string) (provider.Episode, error)
}

func (p lookupTestProvider) LookupEpisode(ctx context.Context, anime provider.Anime, number int, translation string) (provider.Episode, error) {
	return p.lookup(ctx, anime, number, translation)
}

func TestProviderRaceLooksUpOnlyMissingEpisodes(t *testing.T) {
	for _, test := range []struct {
		name, reason string
		listed       bool
		episode      provider.Episode
		lookupErr    error
		streamErr    error
	}{
		{name: "stale catalogue", episode: provider.Episode{ShowID: "61316", Number: 19, Value: "19"}},
		{name: "already listed", listed: true},
		{name: "lookup unavailable", reason: "episode_unavailable", lookupErr: errors.New("secret upstream error")},
		{name: "wrong episode", reason: "episodes_failed", episode: provider.Episode{ShowID: "61316", Number: 9, Value: "9"}},
		{name: "missing show", reason: "episodes_failed", episode: provider.Episode{Number: 19, Value: "19"}},
		{name: "missing value", reason: "episodes_failed", episode: provider.Episode{ShowID: "61316", Number: 19}},
		{name: "unplayable route", reason: "streams_failed", episode: provider.Episode{ShowID: "61316", Number: 19, Value: "19"}, streamErr: errors.New("secret missing media")},
	} {
		t.Run(test.name, func(t *testing.T) {
			var matches, lookups, streams atomic.Int32
			client := lookupTestProvider{raceTestProvider: raceTestProvider{
				match: func(_ context.Context, id int, _ []string, translation string) (provider.Anime, error) {
					matches.Add(1)
					if id != 189046 || translation != "dub" {
						t.Errorf("match arguments: %d/%s", id, translation)
					}
					return provider.Anime{ID: "8327", AniListID: id}, nil
				},
				episodes: func(context.Context, provider.Anime, string) ([]provider.Episode, error) {
					if test.listed {
						return []provider.Episode{{ShowID: "61316", Number: 19, Value: "19"}}, nil
					}
					return []provider.Episode{{ShowID: "61316", Number: 1, Value: "1"}}, nil
				},
				streams: func(_ context.Context, episode provider.Episode, translation, quality string) ([]provider.Stream, error) {
					streams.Add(1)
					if episode != (provider.Episode{ShowID: "61316", Number: 19, Value: "19"}) || translation != "dub" || quality != "720p" {
						t.Errorf("stream arguments: %+v/%s/%s", episode, translation, quality)
					}
					return []provider.Stream{{URL: "https://video.test/19.m3u8"}}, test.streamErr
				},
			}, lookup: func(_ context.Context, anime provider.Anime, number int, translation string) (provider.Episode, error) {
				lookups.Add(1)
				if anime.ID != "8327" || anime.AniListID != 189046 || number != 19 || translation != "dub" {
					t.Errorf("lookup arguments: %+v/%d/%s", anime, number, translation)
				}
				return test.episode, test.lookupErr
			}}
			config := DefaultConfig()
			config.Providers, config.Translation, config.PreferredQuality = []string{"anipub"}, "dub", "720p"
			metadata := &cachingAniList{media: &anilist.Media{ID: 189046, Title: "Re:ZERO Season 4"}}
			var logs bytes.Buffer
			service := NewServiceWithProviders(metadata, map[string]provider.Client{"anipub": client}, nil, nil, nil, nil, config, log.New(&logs, "", 0))
			t.Cleanup(service.Close)
			stream, episode, err := service.resolveStream(t.Context(), Episode{MediaID: 189046, Number: 19})
			service.Close()
			if test.reason != "" {
				if err == nil || err.Error() != "resolve episode 19 stream: anipub: "+test.reason || stream.URL != "" {
					t.Fatalf("failure = %+v, %v", stream, err)
				}
			} else if err != nil || stream.URL != "https://video.test/19.m3u8" || episode != (Episode{
				MediaID: 189046, Number: 19, Title: "Re:ZERO Season 4 - Episode 19", Provider: "anipub", ProviderID: "61316", Value: "19",
			}) {
				t.Fatalf("resolution = %+v, %+v, %v", stream, episode, err)
			}
			wantLookups, wantStreams := int32(1), int32(1)
			if test.listed {
				wantLookups = 0
			}
			if test.reason != "" && test.reason != "streams_failed" {
				wantStreams = 0
			}
			if matches.Load() != 1 || lookups.Load() != wantLookups || streams.Load() != wantStreams {
				t.Fatalf("calls: matches=%d lookups=%d streams=%d", matches.Load(), lookups.Load(), streams.Load())
			}
			if strings.Contains(logs.String(), "secret") {
				t.Fatalf("unsafe lookup logs: %s", logs.String())
			}
		})
	}
}

func TestProviderRaceCancelsMissingEpisodeLookup(t *testing.T) {
	started := make(chan struct{})
	var streams atomic.Int32
	missing := lookupTestProvider{raceTestProvider: raceTestProvider{
		stackTestProvider: stackTestProvider{numbers: []int{1}},
		streams: func(context.Context, provider.Episode, string, string) ([]provider.Stream, error) {
			streams.Add(1)
			return []provider.Stream{{URL: "https://video.test/late"}}, nil
		},
	}, lookup: func(ctx context.Context, _ provider.Anime, number int, _ string) (provider.Episode, error) {
		close(started)
		<-ctx.Done()
		// A late result from an adapter that ignores cancellation must not play.
		return provider.Episode{ShowID: "61316", Number: number, Value: "19"}, nil
	}}
	quick := raceTestProvider{
		stackTestProvider: stackTestProvider{numbers: []int{19}},
		streams: func(ctx context.Context, _ provider.Episode, _, _ string) ([]provider.Stream, error) {
			select {
			case <-started:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return []provider.Stream{{URL: "https://video.test/quick"}}, nil
		},
	}
	config := DefaultConfig()
	config.Providers = []string{"missing", "quick"}
	service := NewServiceWithProviders(&cachingAniList{}, map[string]provider.Client{"missing": missing, "quick": quick}, nil, nil, nil, nil, config, nil)
	t.Cleanup(service.Close)
	result := awaitRace(t, resolveRaceAsync(service, t.Context(), Episode{MediaID: 189046, Number: 19}))
	service.Close()
	if result.err != nil || result.episode.Provider != "quick" || streams.Load() != 0 {
		t.Fatalf("cancelled lookup was used: %+v, streams=%d", result, streams.Load())
	}
}
