package anime

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"

	"tarragon-anime/internal/mpv"
	"tarragon-anime/internal/provider"
)

type stackTestProvider struct {
	name       string
	episodeErr error
	streamErr  error
	streamURL  string
}

func (p stackTestProvider) Match(context.Context, int, []string, string) (provider.Anime, error) {
	return provider.Anime{ID: p.name + "-show", Name: "Frieren"}, nil
}

func (p stackTestProvider) Episodes(context.Context, provider.Anime, string) ([]provider.Episode, error) {
	if p.episodeErr != nil {
		return nil, p.episodeErr
	}
	return []provider.Episode{{ShowID: p.name + "-show", Number: 1, Value: p.name + "-episode"}}, nil
}

func (p stackTestProvider) Streams(context.Context, provider.Episode, string, string) ([]provider.Stream, error) {
	if p.streamErr != nil {
		return nil, p.streamErr
	}
	return []provider.Stream{{URL: p.streamURL}}, nil
}

type stackTestPlayer struct {
	session *fakeSession
	stream  mpv.Stream
}

func (p *stackTestPlayer) Play(_ context.Context, stream mpv.Stream, _ string, _ float64) (mpv.SessionController, error) {
	p.stream = stream
	return p.session, nil
}

func TestEpisodesFallsBackInConfiguredOrder(t *testing.T) {
	config := DefaultConfig()
	config.Providers = []string{"allanime", "animepahe"}
	service := NewServiceWithProviders(&cachingAniList{}, map[string]provider.Client{
		"allanime":  stackTestProvider{name: "allanime", episodeErr: errors.New("unavailable")},
		"animepahe": stackTestProvider{name: "animepahe"},
	}, unusedPlayer{}, nil, nil, nil, config, nil)

	episodes, err := service.Episodes(t.Context(), 154587)
	if err != nil {
		t.Fatal(err)
	}
	if len(episodes) != 1 || episodes[0].Provider != "animepahe" {
		t.Fatalf("Episodes() = %#v", episodes)
	}
}

func TestStreamFallbackLogsServingProvider(t *testing.T) {
	config := DefaultConfig()
	config.Providers = []string{"allanime", "animepahe"}
	var logs bytes.Buffer
	player := &stackTestPlayer{session: newFakeSession()}
	service := NewServiceWithProviders(&cachingAniList{}, map[string]provider.Client{
		"allanime":  stackTestProvider{name: "allanime", streamErr: errors.New("stream unavailable")},
		"animepahe": stackTestProvider{name: "animepahe", streamURL: "https://video.test/animepahe.m3u8"},
	}, player, nil, nil, nil, config, log.New(&logs, "", 0))
	defer service.Close()

	episodes, err := service.Episodes(t.Context(), 154587)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.PlayEpisode(t.Context(), episodes[0]); err != nil {
		t.Fatal(err)
	}
	if player.stream.URL != "https://video.test/animepahe.m3u8" {
		t.Fatalf("mpv stream = %#v", player.stream)
	}
	if !strings.Contains(logs.String(), "stream served provider=animepahe") {
		t.Fatalf("logs do not identify serving provider: %s", logs.String())
	}
}

func TestStreamDoesNotUseUnconfiguredProvider(t *testing.T) {
	config := DefaultConfig()
	player := &stackTestPlayer{session: newFakeSession()}
	service := NewServiceWithProviders(&cachingAniList{}, map[string]provider.Client{
		"allanime":  stackTestProvider{name: "allanime", streamErr: errors.New("stream unavailable")},
		"animepahe": stackTestProvider{name: "animepahe", streamURL: "https://video.test/animepahe.m3u8"},
	}, player, nil, nil, nil, config, nil)
	defer service.Close()

	episodes, err := service.Episodes(t.Context(), 154587)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.PlayEpisode(t.Context(), episodes[0]); err == nil {
		t.Fatal("PlayEpisode() used a provider that was not configured")
	}
	if player.stream.URL != "" {
		t.Fatalf("mpv stream = %#v", player.stream)
	}
}

var _ aniListClient = (*cachingAniList)(nil)
