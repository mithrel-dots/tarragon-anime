package anilist

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSearchMapsGraphQLMedia(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.Header.Get("Content-Type"))
		}
		var request struct {
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request.Variables["search"] != "frieren" {
			t.Fatalf("search variable = %#v", request.Variables["search"])
		}
		_, _ = w.Write([]byte(`{"data":{"Page":{"media":[{"_id":154587,"idMal":52991,"title":{"romaji":"Sousou no Frieren","english":"Frieren: Beyond Journey's End","native":"葬送のフリーレン"},"synonyms":["Frieren"],"format":"TV","episodes":28,"coverImage":{"large":"https://img.test/cover.jpg"}}]}}}`))
	}))
	defer server.Close()

	media, err := NewClientWithEndpoint(server.Client(), server.URL).Search(context.Background(), "frieren")
	if err != nil {
		t.Fatal(err)
	}
	if len(media) != 1 || media[0].ID != 154587 || media[0].IDMal != 52991 || media[0].Title != "Frieren: Beyond Journey's End" || media[0].Episodes != 28 {
		t.Fatalf("Search() = %#v", media)
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

func TestAuthenticatedProgressOperations(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Fatalf("Authorization = %q", r.Header.Get("Authorization"))
		}
		var request struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
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
	if err != nil {
		t.Fatal(err)
	}
	if !found || entry.Progress != 3 {
		t.Fatalf("ListEntry() = %#v, %v", entry, found)
	}
	entry, err = client.SaveProgress(t.Context(), 154587, 4, "CURRENT")
	if err != nil {
		t.Fatal(err)
	}
	if entry.Progress != 4 || requests != 2 {
		t.Fatalf("SaveProgress() = %#v, requests = %d", entry, requests)
	}
}

func TestAuthenticatedListOperations(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Fatalf("Authorization = %q", r.Header.Get("Authorization"))
		}
		var request struct {
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if requests == 1 {
			if request.Variables["userId"] != float64(1) {
				t.Fatalf("userId variable = %#v", request.Variables["userId"])
			}
			if request.Variables["status"] != "CURRENT" {
				t.Fatalf("status variable = %#v", request.Variables["status"])
			}
			_, _ = w.Write([]byte(`{"data":{"MediaListCollection":{"lists":[{"entries":[{"status":"CURRENT","progress":4,"media":{"_id":154587,"title":{"english":"Frieren"},"format":"TV","episodes":28,"coverImage":{"large":"https://img.test/cover.jpg"}}}]}]}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"SaveMediaListEntry":{"id":9}}}`))
	}))
	defer server.Close()
	client := NewAuthenticatedClientWithEndpoint(server.Client(), server.URL, func() (string, error) { return "test-token", nil })
	items, err := client.List(t.Context(), 1, "CURRENT")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Media.ID != 154587 || items[0].Progress != 4 {
		t.Fatalf("List() = %#v", items)
	}
	if err := client.SetStatus(t.Context(), 154587, "COMPLETED"); err != nil {
		t.Fatal(err)
	}
}
