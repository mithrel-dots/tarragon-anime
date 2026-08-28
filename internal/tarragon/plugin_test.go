package tarragon

import (
	"context"
	"io"
	"log"
	"testing"

	"tarragon-anime/internal/anime"
)

type fakeAnimeService struct {
	played    anime.Episode
	resumed   int
	signedIn  bool
	loggedIn  bool
	loggedOut bool
	list      []anime.ListItem
	status    string
	opened    int
}

func (f *fakeAnimeService) SignedIn() bool { return f.signedIn }

func (f *fakeAnimeService) Account(context.Context) string {
	if f.signedIn {
		return "mithrel"
	}
	return ""
}

func (f *fakeAnimeService) StartLogin(context.Context) error {
	f.loggedIn = true
	return nil
}

func (f *fakeAnimeService) Logout(context.Context) error {
	f.loggedOut = true
	return nil
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

func (f *fakeAnimeService) List(context.Context, string) ([]anime.ListItem, error) {
	return f.list, nil
}

func (f *fakeAnimeService) SetListStatus(_ context.Context, _ int, status string) error {
	f.status = status
	return nil
}

func (f *fakeAnimeService) OpenMedia(_ context.Context, mediaID int) error {
	f.opened = mediaID
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
	if len(payload.Results) != 2 {
		t.Fatalf("results = %#v", payload.Results)
	}
	if signIn := payload.Results[1]; signIn.ID != "auth:login" || signIn.Actions[0].Name != "login" {
		t.Fatalf("sign-in result = %#v", signIn)
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
	if payload.Results[0].Actions[len(payload.Results[0].Actions)-1].Name != "open" {
		t.Fatalf("actions = %#v", payload.Results[0].Actions)
	}
	success, _ := plugin.Select(t.Context(), Message{
		QueryID: "search-query", ResultID: "media:154587", Action: "resume", Plugin: "anime",
	})
	if !success || service.resumed != 154587 {
		t.Fatalf("Select() = %v, resumed %d", success, service.resumed)
	}
}

func TestPluginAuthenticatedMediaActions(t *testing.T) {
	service := &fakeAnimeService{signedIn: true}
	plugin := NewPlugin(service, "anime", "anime", "@", log.New(io.Discard, "", 0))
	payload := plugin.Request(t.Context(), "search-query", "frieren")
	if len(payload.Results[0].Actions) != 6 {
		t.Fatalf("actions = %#v", payload.Results[0].Actions)
	}
	for _, action := range []string{"watching", "planning", "completed", "open"} {
		success, _ := plugin.Select(t.Context(), Message{
			QueryID: "search-query", ResultID: "media:154587", Action: action, Plugin: "anime",
		})
		if !success {
			t.Fatalf("%s action failed", action)
		}
	}
	if service.status != "COMPLETED" || service.opened != 154587 {
		t.Fatalf("status=%q opened=%d", service.status, service.opened)
	}
}

func TestPluginListQuery(t *testing.T) {
	service := &fakeAnimeService{
		signedIn: true,
		list: []anime.ListItem{{
			Media:  anime.Media{ID: 154587, Title: "Frieren", Episodes: 28},
			Status: "CURRENT", Progress: 4,
		}},
	}
	plugin := NewPlugin(service, "anime", "anime", "@", log.New(io.Discard, "", 0))
	payload := plugin.Request(t.Context(), "list-query", "list watching")
	if len(payload.Results) != 1 || payload.Results[0].Description != "watching | episode 4 of 28" {
		t.Fatalf("list payload = %#v", payload.Results)
	}
}

func TestPluginLoginAndLogoutActions(t *testing.T) {
	service := &fakeAnimeService{}
	plugin := NewPlugin(service, "anime", "anime", "@", log.New(io.Discard, "", 0))

	login := plugin.Request(t.Context(), "login-query", "login")
	if login.Results[0].ID != "auth:login" || login.Results[0].Actions[0].Name != "login" {
		t.Fatalf("login payload = %#v", login.Results)
	}
	success, message := plugin.Select(t.Context(), Message{
		QueryID: "login-query", ResultID: "auth:login", Action: "login", Plugin: "anime",
	})
	if !success || message != "Opened AniList sign-in" || !service.loggedIn {
		t.Fatalf("login select = %v, %q, started %v", success, message, service.loggedIn)
	}

	if payload := plugin.Request(t.Context(), "logout-query", "logout"); len(payload.Results[0].Actions) != 0 {
		t.Fatalf("signed-out logout payload = %#v", payload.Results)
	}
	service.signedIn = true
	logout := plugin.Request(t.Context(), "logout-query", "logout")
	if logout.Results[0].Actions[0].Name != "logout" {
		t.Fatalf("logout payload = %#v", logout.Results)
	}
	success, _ = plugin.Select(t.Context(), Message{
		QueryID: "logout-query", ResultID: "auth:logout", Action: "logout", Plugin: "anime",
	})
	if !success || !service.loggedOut {
		t.Fatalf("logout select = %v, signed out %v", success, service.loggedOut)
	}
}

func TestPluginHidesSignInWhenAuthenticated(t *testing.T) {
	service := &fakeAnimeService{signedIn: true}
	plugin := NewPlugin(service, "anime", "anime", "@", log.New(io.Discard, "", 0))
	payload := plugin.Request(t.Context(), "empty-query", "")
	if len(payload.Results) != 2 || payload.Results[0].ID != "media:154587" {
		t.Fatalf("results = %#v", payload.Results)
	}
	status := payload.Results[1]
	if status.ID != "auth:status" || len(status.Actions) != 0 || status.Description != "Signed in as mithrel" {
		t.Fatalf("status result = %#v", status)
	}
	success, message := plugin.Select(t.Context(), Message{
		QueryID: "empty-query", ResultID: "auth:status", Action: "", Plugin: "anime",
	})
	if !success || message != "Already signed in to AniList" {
		t.Fatalf("Select() = %v, %q", success, message)
	}
	if service.loggedIn || service.loggedOut {
		t.Fatal("status selection changed authentication state")
	}
}

func TestPluginSignInFromEmptyQueryStartsLogin(t *testing.T) {
	service := &fakeAnimeService{}
	plugin := NewPlugin(service, "anime", "anime", "@", log.New(io.Discard, "", 0))
	plugin.Request(t.Context(), "empty-query", "")
	success, message := plugin.Select(t.Context(), Message{
		QueryID: "empty-query", ResultID: "auth:login", Action: "login", Plugin: "anime",
	})
	if !success || message != "Opened AniList sign-in" || !service.loggedIn {
		t.Fatalf("Select() = %v, %q, started %v", success, message, service.loggedIn)
	}
}
