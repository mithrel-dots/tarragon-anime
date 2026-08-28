package tarragon

import (
	"context"
	"io"
	"log"
	"testing"

	"tarragon-anime/internal/anime"
)

type fakeAnimeService struct {
	played anime.Episode
}

func (f *fakeAnimeService) Search(context.Context, string) ([]anime.Media, error) {
	return []anime.Media{{ID: 154587, Title: "Frieren", Format: "TV", Episodes: 28}}, nil
}

func (f *fakeAnimeService) Episodes(context.Context, int) ([]anime.Episode, error) {
	return []anime.Episode{{MediaID: 154587, Number: 4, Title: "Frieren - Episode 4", Provider: "allanime", ProviderID: "show-1"}}, nil
}

func (f *fakeAnimeService) Play(context.Context, int, int) error { return nil }

func (f *fakeAnimeService) PlayEpisode(_ context.Context, episode anime.Episode) error {
	f.played = episode
	return nil
}

func TestPluginQueryReplacementAndSelection(t *testing.T) {
	service := &fakeAnimeService{}
	plugin := NewPlugin(service, "custom-plugin", "shows", "@", log.New(io.Discard, "", 0))
	search := plugin.Request(t.Context(), "search-query", "frieren")
	if len(search.Results) != 1 {
		t.Fatalf("search results = %#v", search.Results)
	}
	action := search.Results[0].Actions[0]
	if action.Type != "query_replace" || action.Query != "@shows episodes 154587" {
		t.Fatalf("episodes action = %#v", action)
	}

	episodes := plugin.Request(t.Context(), "episodes-query", "episodes 154587")
	if len(episodes.Results) != 1 || episodes.Results[0].ID != "episode:154587:4" || episodes.Results[0].Actions[0].Name != "play" {
		t.Fatalf("episode results = %#v", episodes.Results)
	}
	success, message := plugin.Select(t.Context(), Message{
		Type: "select", QueryID: "episodes-query", ResultID: "episode:154587:4", Action: "play", Plugin: "custom-plugin",
	})
	if !success || message != "Started playback" || service.played.Number != 4 {
		t.Fatalf("Select() = %v, %q, played %#v", success, message, service.played)
	}
}

func TestPluginRejectsStaleSelection(t *testing.T) {
	plugin := NewPlugin(&fakeAnimeService{}, "custom-plugin", "shows", "@", log.New(io.Discard, "", 0))
	success, _ := plugin.Select(t.Context(), Message{QueryID: "old", ResultID: "episode:1:1", Action: "play"})
	if success {
		t.Fatal("Select() accepted stale selection")
	}
}
