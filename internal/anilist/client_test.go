package anilist

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func assertRequest(t *testing.T, clientErr error, requestErrors <-chan error) {
	t.Helper()
	if clientErr != nil {
		select {
		case requestErr := <-requestErrors:
			if requestErr != nil {
				t.Fatalf("request handler: %v; client: %v", requestErr, clientErr)
			}
		default:
		}
		t.Fatal(clientErr)
	}
	select {
	case requestErr := <-requestErrors:
		if requestErr != nil {
			t.Fatal(requestErr)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for request handler")
	}
}

func TestSearchMapsGraphQLMedia(t *testing.T) {
	requestErrors := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var handlerErr error
		defer func() { requestErrors <- handlerErr }()
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			handlerErr = fmt.Errorf("unexpected request: %s %s", r.Method, r.Header.Get("Content-Type"))
			http.Error(w, handlerErr.Error(), http.StatusBadRequest)
			return
		}
		var request struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			handlerErr = fmt.Errorf("decode request: %w", err)
			http.Error(w, handlerErr.Error(), http.StatusBadRequest)
			return
		}
		if request.Variables["search"] != "frieren" {
			handlerErr = fmt.Errorf("search variable = %#v", request.Variables["search"])
			http.Error(w, handlerErr.Error(), http.StatusBadRequest)
			return
		}
		if !strings.Contains(request.Query, "episodes status nextAiringEpisode { episode }") || strings.Contains(request.Query, "airingSchedule") {
			handlerErr = fmt.Errorf("unexpected search media fields: %s", request.Query)
			http.Error(w, handlerErr.Error(), http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"data":{"Page":{"media":[{"_id":154587,"idMal":52991,"title":{"romaji":"Sousou no Frieren","english":"Frieren: Beyond Journey's End","native":"葬送のフリーレン"},"synonyms":["Frieren"],"format":"TV","episodes":28,"status":"FINISHED","nextAiringEpisode":null,"coverImage":{"large":"https://img.test/cover.jpg"}}]}}}`))
	}))
	defer server.Close()

	media, err := NewClientWithEndpoint(server.Client(), server.URL).Search(context.Background(), "frieren")
	assertRequest(t, err, requestErrors)
	if len(media) != 1 || media[0].ID != 154587 || media[0].IDMal != 52991 || media[0].Title != "Frieren: Beyond Journey's End" || media[0].Episodes != 28 || media[0].AiredEpisodes != 28 {
		t.Fatalf("Search() = %#v", media)
	}
}

func TestGetMapsAiredEpisodes(t *testing.T) {
	tests := []struct {
		name          string
		media         string
		schedules     string
		episodes      int
		airedEpisodes int
	}{
		{"releasing 23 of 24", `{"_id":182205,"status":"RELEASING","episodes":24,"nextAiringEpisode":{"episode":24}}`, `[{"episode":23}]`, 24, 23},
		{"finished known total", `{"status":"FINISHED","episodes":24,"nextAiringEpisode":null}`, `[]`, 24, 24},
		{"finished total overrides stale schedule", `{"status":"FINISHED","episodes":24}`, `[{"episode":23}]`, 24, 24},
		{"planned", `{"status":"NOT_YET_RELEASED","episodes":24}`, `[]`, 24, 0},
		{"planned overrides inconsistent schedule", `{"status":"NOT_YET_RELEASED","episodes":24}`, `[{"episode":1}]`, 24, 0},
		{"unknown total with next", `{"status":"RELEASING","episodes":null,"nextAiringEpisode":{"episode":7}}`, `[]`, 0, 6},
		{"missing next with aired schedule", `{"status":"RELEASING","episodes":24,"nextAiringEpisode":null}`, `[{"episode":23}]`, 24, 23},
		{"unknown total with aired schedule", `{}`, `[{"episode":6}]`, 0, 6},
		{"finished unknown total with schedule", `{"status":"FINISHED","episodes":null}`, `[{"episode":12}]`, 0, 12},
		{"finished unknown total without schedule", `{"status":"FINISHED","episodes":null}`, `[]`, 0, -1},
		{"finished ignores inconsistent next", `{"status":"FINISHED","nextAiringEpisode":{"episode":24}}`, `[]`, 0, -1},
		{"unknown metadata", `{}`, `[]`, 0, -1},
		{"null metadata", `{"status":null,"episodes":null,"nextAiringEpisode":null}`, `null`, 0, -1},
		{"releasing total alone", `{"status":"RELEASING","episodes":24}`, `[]`, 24, -1},
		{"hiatus total alone", `{"status":"HIATUS","episodes":24}`, `[]`, 24, -1},
		{"cancelled total alone", `{"status":"CANCELLED","episodes":24}`, `[]`, 24, -1},
		{"unknown status total alone", `{"status":"UNKNOWN","episodes":24}`, `[]`, 24, -1},
		{"first episode upcoming", `{"status":"RELEASING","episodes":24,"nextAiringEpisode":{"episode":1}}`, `[]`, 24, 0},
		{"zero next episode", `{"status":"RELEASING","episodes":24,"nextAiringEpisode":{"episode":0}}`, `[]`, 24, -1},
		{"negative next episode", `{"status":"RELEASING","episodes":24,"nextAiringEpisode":{"episode":-2}}`, `[]`, 24, -1},
		{"negative total does not cap", `{"status":"RELEASING","episodes":-2,"nextAiringEpisode":{"episode":7}}`, `[]`, -2, 6},
		{"invalid schedules", `{"status":"RELEASING"}`, `[null,{}, {"episode":0},{"episode":-2}]`, 0, -1},
		{"invalid schedule falls back to next", `{"status":"RELEASING","nextAiringEpisode":{"episode":7}}`, `[{"episode":0}]`, 0, 6},
		{"schedule precedes next estimate", `{"status":"RELEASING","episodes":24,"nextAiringEpisode":{"episode":24}}`, `[{"episode":12}]`, 24, 12},
		{"schedule capped by total", `{"status":"RELEASING","episodes":24}`, `[{"episode":30}]`, 24, 24},
		{"next estimate capped by total", `{"status":"RELEASING","episodes":24,"nextAiringEpisode":{"episode":30}}`, `[]`, 24, 24},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requestErrors := make(chan error, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var handlerErr error
				defer func() { requestErrors <- handlerErr }()
				var request struct {
					Query     string         `json:"query"`
					Variables map[string]any `json:"variables"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					handlerErr = fmt.Errorf("decode request: %w", err)
					http.Error(w, handlerErr.Error(), http.StatusBadRequest)
					return
				}
				if request.Variables["id"] != float64(182205) ||
					!strings.Contains(request.Query, "episodes status nextAiringEpisode { episode }") ||
					!strings.Contains(request.Query, "} Page(page: 1, perPage: 1) { airingSchedules(mediaId: $id, notYetAired: false, sort: EPISODE_DESC) { episode } } }") ||
					strings.Contains(request.Query, "airingSchedule(") {
					handlerErr = fmt.Errorf("unexpected Get request: %#v", request)
					http.Error(w, handlerErr.Error(), http.StatusBadRequest)
					return
				}
				_, _ = fmt.Fprintf(w, `{"data":{"Media":%s,"Page":{"airingSchedules":%s}}}`, tt.media, tt.schedules)
			}))
			defer server.Close()

			media, err := NewClientWithEndpoint(server.Client(), server.URL).Get(t.Context(), 182205)
			assertRequest(t, err, requestErrors)
			if media.Episodes != tt.episodes || media.AiredEpisodes != tt.airedEpisodes {
				t.Fatalf("Get() episodes = %d, aired = %d; want episodes = %d, aired = %d", media.Episodes, media.AiredEpisodes, tt.episodes, tt.airedEpisodes)
			}
		})
	}
}

func TestGraphQLError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"errors":[{"message":"rate limited"}]}`))
	}))
	defer server.Close()
	_, err := NewClientWithEndpoint(server.Client(), server.URL).Search(context.Background(), "x")
	if err == nil {
		t.Fatal("Search() error = nil")
	}
}

func TestGraphQLHTTPErrorIncludesMessage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"errors":[{"message":"API temporarily disabled"}]}`))
	}))
	defer server.Close()

	_, err := NewClientWithEndpoint(server.Client(), server.URL).Search(context.Background(), "x")
	if err == nil || !strings.Contains(err.Error(), "403 Forbidden: API temporarily disabled") {
		t.Fatalf("Search() error = %v", err)
	}
}

func TestAuthenticatedProgressOperations(t *testing.T) {
	requests := 0
	requestErrors := make(chan error, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var handlerErr error
		defer func() { requestErrors <- handlerErr }()
		requests++
		if r.Header.Get("Authorization") != "Bearer test-token" {
			handlerErr = fmt.Errorf("Authorization = %q", r.Header.Get("Authorization"))
			http.Error(w, handlerErr.Error(), http.StatusUnauthorized)
			return
		}
		var request struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			handlerErr = fmt.Errorf("decode request: %w", err)
			http.Error(w, handlerErr.Error(), http.StatusBadRequest)
			return
		}
		if request.Variables["progress"] != nil {
			_, _ = w.Write([]byte(`{"data":{"SaveMediaListEntry":{"id":9,"status":"CURRENT","progress":4}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"Media":{"mediaListEntry":{"id":9,"status":"CURRENT","progress":3}}}}`))
	}))
	defer server.Close()
	client := NewAuthenticatedClientWithEndpoint(server.Client(), server.URL, func() (string, error) { return "test-token", nil })
	entry, found, err := client.ListEntry(t.Context(), 154587)
	assertRequest(t, err, requestErrors)
	if !found || entry.Progress != 3 {
		t.Fatalf("ListEntry() = %#v, %v", entry, found)
	}
	entry, err = client.SaveProgress(t.Context(), 154587, 4, "CURRENT")
	assertRequest(t, err, requestErrors)
	if entry.Progress != 4 || requests != 2 {
		t.Fatalf("SaveProgress() = %#v, requests = %d", entry, requests)
	}
}

func TestAuthenticatedListOperations(t *testing.T) {
	requests := 0
	requestErrors := make(chan error, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var handlerErr error
		defer func() { requestErrors <- handlerErr }()
		requests++
		if r.Header.Get("Authorization") != "Bearer test-token" {
			handlerErr = fmt.Errorf("Authorization = %q", r.Header.Get("Authorization"))
			http.Error(w, handlerErr.Error(), http.StatusUnauthorized)
			return
		}
		var request struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			handlerErr = fmt.Errorf("decode request: %w", err)
			http.Error(w, handlerErr.Error(), http.StatusBadRequest)
			return
		}
		if requests == 1 {
			if request.Variables["userId"] != float64(1) {
				handlerErr = fmt.Errorf("userId variable = %#v", request.Variables["userId"])
				http.Error(w, handlerErr.Error(), http.StatusBadRequest)
				return
			}
			if request.Variables["status"] != "CURRENT" {
				handlerErr = fmt.Errorf("status variable = %#v", request.Variables["status"])
				http.Error(w, handlerErr.Error(), http.StatusBadRequest)
				return
			}
			if !strings.Contains(request.Query, "episodes status nextAiringEpisode { episode }") || strings.Contains(request.Query, "airingSchedule") {
				handlerErr = fmt.Errorf("unexpected list media fields: %s", request.Query)
				http.Error(w, handlerErr.Error(), http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`{"data":{"MediaListCollection":{"lists":[{"entries":[{"status":"CURRENT","progress":4,"media":{"_id":154587,"title":{"english":"Frieren"},"format":"TV","episodes":28,"status":"RELEASING","nextAiringEpisode":{"episode":24},"coverImage":{"large":"https://img.test/cover.jpg"}}}]}]}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"SaveMediaListEntry":{"id":9}}}`))
	}))
	defer server.Close()
	client := NewAuthenticatedClientWithEndpoint(server.Client(), server.URL, func() (string, error) { return "test-token", nil })
	items, err := client.List(t.Context(), 1, "CURRENT")
	assertRequest(t, err, requestErrors)
	if len(items) != 1 || items[0].Media.ID != 154587 || items[0].Progress != 4 || items[0].Media.Episodes != 28 || items[0].Media.AiredEpisodes != 23 {
		t.Fatalf("List() = %#v", items)
	}
	err = client.SetStatus(t.Context(), 154587, "COMPLETED")
	assertRequest(t, err, requestErrors)
}

func TestAuthenticatedUnfilteredList(t *testing.T) {
	requestErrors := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var handlerErr error
		defer func() { requestErrors <- handlerErr }()
		var request struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			handlerErr = err
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if _, found := request.Variables["status"]; found || strings.Contains(request.Query, "$status") {
			handlerErr = fmt.Errorf("unfiltered list request included status: %#v", request)
			http.Error(w, handlerErr.Error(), http.StatusBadRequest)
			return
		}
		if !strings.Contains(request.Query, "episodes status nextAiringEpisode { episode }") || strings.Contains(request.Query, "airingSchedule") {
			handlerErr = fmt.Errorf("unexpected unfiltered list media fields: %s", request.Query)
			http.Error(w, handlerErr.Error(), http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"data":{"MediaListCollection":{"lists":[{"entries":[{"status":"CURRENT","progress":4,"media":{"_id":154587,"title":{"english":"Frieren"},"episodes":28}}]}]}}}`))
	}))
	defer server.Close()

	client := NewAuthenticatedClientWithEndpoint(server.Client(), server.URL, func() (string, error) { return "test-token", nil })
	items, err := client.List(t.Context(), 1, "")
	assertRequest(t, err, requestErrors)
	if len(items) != 1 || items[0].Media.ID != 154587 || items[0].Progress != 4 || items[0].Media.AiredEpisodes != -1 {
		t.Fatalf("List() = %#v", items)
	}
}
