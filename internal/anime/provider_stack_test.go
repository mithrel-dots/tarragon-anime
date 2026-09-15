package anime

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	browserpkg "tarragon-anime/internal/browser"
	"tarragon-anime/internal/mpv"
	"tarragon-anime/internal/provider"
	"tarragon-anime/internal/provider/anipub"
)

type stackTestProvider struct {
	name        string
	episodeErr  error
	streamErr   error
	streamURL   string
	emptyShowID bool
}

type stackCaptureFunc func(context.Context, browserpkg.Request) ([]browserpkg.Candidate, error)

func (f stackCaptureFunc) Capture(ctx context.Context, request browserpkg.Request) ([]browserpkg.Candidate, error) {
	return f(ctx, request)
}

func (p stackTestProvider) Match(context.Context, int, []string, string) (provider.Anime, error) {
	return provider.Anime{ID: p.name + "-show", Name: "Frieren"}, nil
}

func (p stackTestProvider) Episodes(context.Context, provider.Anime, string) ([]provider.Episode, error) {
	if p.episodeErr != nil {
		return nil, p.episodeErr
	}
	showID := p.name + "-show"
	if p.emptyShowID {
		showID = ""
	}
	return []provider.Episode{{ShowID: showID, Number: 1, Value: p.name + "-episode"}}, nil
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
	if len(episodes) != 1 || episodes[0].ProviderID != "59970" {
		t.Fatalf("Episodes() = %#v, want playback provider id 59970", episodes)
	}
	mapping, found, err := state.ProviderMapping(t.Context(), 154587, "anipub")
	if err != nil || !found || mapping != "8347" {
		t.Fatalf("provider mapping = %q, %v, %v; want 8347, true, nil", mapping, found, err)
	}
	if err := service.PlayEpisode(t.Context(), episodes[0]); err == nil {
		t.Fatal("PlayEpisode() succeeded, want the player failure")
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
	if len(episodes) != 1 || episodes[0].ProviderID != "catalogue-show" {
		t.Fatalf("Episodes() = %#v, want the catalogue provider id catalogue-show", episodes)
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
