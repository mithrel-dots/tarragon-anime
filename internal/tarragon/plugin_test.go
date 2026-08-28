package tarragon

import (
	"context"
	"io"
	"log"
	"testing"

	"tarragon-anime/internal/anime"
)

type fakeAnimeService struct {
	played  anime.Episode
	resumed int
}

func (f *fakeAnimeService) ContinueWatching(context.Context, int) ([]anime.Resume, error) {
	return []anime.Resume{{
		MediaID: 154587, Title: "Frieren", Episode: 5, TotalEpisodes: 28, Continues: false,
	}}, nil
}

func (f *fakeAnimeService) ResumeMedia(_ context.Context, mediaID int) error {
	f.resumed = mediaID
	return nil
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
	action := search.Results[0].Actions[1]
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

func TestPluginContinueWatchingIsDefaultForEmptyQuery(t *testing.T) {
	service := &fakeAnimeService{}
	plugin := NewPlugin(service, "anime", "anime", "@", log.New(io.Discard, "", 0))
	payload := plugin.Request(t.Context(), "empty-query", "")
	if len(payload.Results) != 1 {
		t.Fatalf("results = %#v", payload.Results)
	}
	result := payload.Results[0]
	if result.ID != "media:154587" || result.Description != "Next episode 5 of 28" {
		t.Fatalf("result = %#v", result)
	}
	if result.Actions[0].Name != "resume" || result.Actions[1].Query != "@anime episodes 154587" {
		t.Fatalf("actions = %#v", result.Actions)
	}
	success, message := plugin.Select(t.Context(), Message{
		QueryID: "empty-query", ResultID: "media:154587", Action: "resume", Plugin: "anime",
	})
	if !success || message != "Resumed playback" || service.resumed != 154587 {
		t.Fatalf("Select() = %v, %q, resumed %d", success, message, service.resumed)
	}
}

func TestPluginSearchExposesResumeAction(t *testing.T) {
	service := &fakeAnimeService{}
	plugin := NewPlugin(service, "anime", "anime", "@", log.New(io.Discard, "", 0))
	payload := plugin.Request(t.Context(), "search-query", "frieren")
	if payload.Results[0].Actions[0].Name != "resume" {
		t.Fatalf("actions = %#v", payload.Results[0].Actions)
	}
	success, _ := plugin.Select(t.Context(), Message{
		QueryID: "search-query", ResultID: "media:154587", Action: "resume", Plugin: "anime",
	})
	if !success || service.resumed != 154587 {
		t.Fatalf("Select() = %v, resumed %d", success, service.resumed)
	}
}
