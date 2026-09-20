package anime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"tarragon-anime/internal/anilist"
	browserpkg "tarragon-anime/internal/browser"
	"tarragon-anime/internal/mpv"
	"tarragon-anime/internal/provider"
	"tarragon-anime/internal/provider/anipub"
)

type stackTestProvider struct {
	name          string
	episodeErr    error
	streamErr     error
	streamURL     string
	emptyShowID   bool
	numbers       []int
	streamEpisode *provider.Episode
	calls         *atomic.Int32
}

type stackCaptureFunc func(context.Context, browserpkg.Request) ([]browserpkg.Candidate, error)

func (f stackCaptureFunc) Capture(ctx context.Context, request browserpkg.Request) ([]browserpkg.Candidate, error) {
	return f(ctx, request)
}

func (p stackTestProvider) Match(context.Context, int, []string, string) (provider.Anime, error) {
	if p.calls != nil {
		p.calls.Add(1)
	}
	return provider.Anime{ID: p.name + "-show", Name: "Frieren"}, nil
}

func (p stackTestProvider) Episodes(context.Context, provider.Anime, string) ([]provider.Episode, error) {
	if p.calls != nil {
		p.calls.Add(1)
	}
	if p.episodeErr != nil {
		return nil, p.episodeErr
	}
	showID := p.name + "-show"
	if p.emptyShowID {
		showID = ""
	}
	if p.numbers == nil {
		return []provider.Episode{{ShowID: showID, Number: 1, Value: p.name + "-episode"}}, nil
	}
	var episodes []provider.Episode
	for _, number := range p.numbers {
		episodes = append(episodes, provider.Episode{ShowID: showID, Number: number, Value: fmt.Sprintf("%s-episode-%d", p.name, number)})
	}
	return episodes, nil
}

func (p stackTestProvider) Streams(_ context.Context, episode provider.Episode, _, _ string) ([]provider.Stream, error) {
	if p.calls != nil {
		p.calls.Add(1)
	}
	if p.streamEpisode != nil {
		*p.streamEpisode = episode
	}
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

func TestPlaybackFallsBackWhenProviderCatalogueFails(t *testing.T) {
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
	_, resolved, err := service.resolveStream(t.Context(), episodes[0])
	if err != nil || resolved.Provider != "animepahe" {
		t.Fatalf("resolveStream() = %#v, %v", resolved, err)
	}
}

func TestEpisodesUseAniListAndResolveProvidersOnlyOnPlayback(t *testing.T) {
	var first, second []int
	for number := 23; number > 0; number-- {
		second = append(second, number)
		if number <= 17 {
			first = append(first, number)
		}
	}
	config := DefaultConfig()
	config.Providers = []string{"animeflix", "missing", "animepahe", "allanime"}
	var played provider.Episode
	var calls atomic.Int32
	player := &stackTestPlayer{session: newFakeSession()}
	metadata := &cachingAniList{media: &anilist.Media{ID: 182205, Title: "Slime Season 4", Episodes: 24, AiredEpisodes: 23}}
	service := NewServiceWithProviders(metadata, map[string]provider.Client{
		"animeflix": stackTestProvider{name: "animeflix", numbers: first, calls: &calls},
		"animepahe": stackTestProvider{name: "animepahe", episodeErr: errors.New("unavailable"), calls: &calls},
		"allanime":  stackTestProvider{name: "allanime", numbers: second, streamEpisode: &played, streamURL: "https://video.test/23.m3u8", calls: &calls},
		"anipub":    stackTestProvider{name: "anipub", numbers: []int{24}, calls: &calls},
	}, player, nil, nil, nil, config, nil)
	defer service.Close()

	episodes, err := service.Episodes(t.Context(), 182205)
	if err != nil {
		t.Fatal(err)
	}
	if len(episodes) != 23 {
		t.Fatalf("Episodes() returned %d episodes, want 23", len(episodes))
	}
	for index, episode := range episodes {
		number := index + 1
		if episode != (Episode{MediaID: 182205, Number: number, Title: fmt.Sprintf("Slime Season 4 - Episode %d", number)}) {
			t.Fatalf("episode at index %d = %#v, want provider-neutral episode %d", index, episode, number)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("listing made %d provider calls", calls.Load())
	}
	if err := service.PlayEpisode(t.Context(), episodes[22]); err != nil {
		t.Fatal(err)
	}
	if played != (provider.Episode{ShowID: "allanime-show", Number: 23, Value: "allanime-episode-23"}) || player.stream.URL != "https://video.test/23.m3u8" {
		t.Fatalf("played = %#v, stream = %#v", played, player.stream)
	}
}

func TestEpisodesRefreshAiringCountWithoutProviders(t *testing.T) {
	config := DefaultConfig()
	config.Providers = []string{}
	metadata := &cachingAniList{media: &anilist.Media{ID: 182205, Title: "Slime Season 4", Episodes: 24}}
	service := NewServiceWithProviders(metadata, nil, unusedPlayer{}, nil, nil, nil, config, nil)
	defer service.Close()

	for _, count := range []int{23, 24, 0} {
		metadata.media.AiredEpisodes = count
		episodes, err := service.Episodes(t.Context(), 182205)
		if err != nil {
			t.Fatal(err)
		}
		if len(episodes) != count {
			t.Fatalf("episode count = %d, want %d", len(episodes), count)
		}
		if len(service.episodes[182205]) != count {
			t.Fatalf("cached count = %d, want %d", len(service.episodes[182205]), count)
		}
	}
}

func TestProviderEpisodeUsesPlaybackShowID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/search/"):
			_, _ = w.Write([]byte(`[{"Name":"Frieren","Id":8347}]`))
		case r.URL.Path == "/v1/api/details/8347":
			_, _ = w.Write([]byte(`{"local":{"link":"src=/play/59970/5/sub"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	client := anipub.NewClientWithBaseURL(server.Client(), server.URL)
	var capturedURL string
	captures := 0
	client.UseBrowser(stackCaptureFunc(func(_ context.Context, request browserpkg.Request) ([]browserpkg.Candidate, error) {
		captures++
		capturedURL = request.PageURL
		return []browserpkg.Candidate{{URL: "https://video.test/episode.m3u8", Status: http.StatusOK}}, nil
	}))
	state := newMemoryState()
	player := &fakePlayer{session: newFakeSession(), err: errors.New("player unavailable")}
	config := DefaultConfig()
	config.Providers = []string{"anipub"}
	service := NewServiceWithProviders(&cachingAniList{}, map[string]provider.Client{"anipub": client}, player, state, nil, nil, config, nil)
	defer service.Close()

	episodes, err := service.Episodes(t.Context(), 154587)
	if err != nil {
		t.Fatal(err)
	}
	if len(episodes) != 28 || episodes[4].ProviderID != "" {
		t.Fatalf("Episodes() = %#v, want provider-neutral episodes", episodes)
	}
	if err := service.PlayEpisode(t.Context(), episodes[4]); err == nil {
		t.Fatal("PlayEpisode() succeeded, want the player failure")
	}
	mapping, found, err := state.ProviderMapping(t.Context(), 154587, "anipub")
	if err != nil || !found || mapping != "8347" {
		t.Fatalf("provider mapping = %q, %v, %v; want 8347, true, nil", mapping, found, err)
	}
	if capturedURL != server.URL+"/play/59970/5/sub" {
		t.Fatalf("captured page = %q, want playback show id", capturedURL)
	}
	if _, err := client.Streams(t.Context(), provider.Episode{ShowID: "59970", Number: 5, Value: "5"}, config.Translation, config.PreferredQuality); err != nil {
		t.Fatal(err)
	}
	if captures != 2 {
		t.Fatalf("captures = %d, want invalidation to discard the playback-id cache", captures)
	}
}

func TestProviderEpisodeFallsBackToCatalogueIDWithoutShowID(t *testing.T) {
	config := DefaultConfig()
	config.Providers = []string{"fallback"}
	service := NewServiceWithProviders(&cachingAniList{}, map[string]provider.Client{
		"fallback": stackTestProvider{name: "catalogue", emptyShowID: true},
	}, unusedPlayer{}, nil, nil, nil, config, nil)

	episodes, err := service.Episodes(t.Context(), 154587)
	if err != nil {
		t.Fatal(err)
	}
	_, resolved, err := service.resolveStream(t.Context(), episodes[0])
	if err != nil || resolved.ProviderID != "catalogue-show" {
		t.Fatalf("resolveStream() = %#v, %v; want the catalogue provider id catalogue-show", resolved, err)
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
