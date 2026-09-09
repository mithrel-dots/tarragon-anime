package allanime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"tarragon-anime/internal/browser"
)

func TestNormalizeTitle(t *testing.T) {
	tests := map[string]string{
		"Frieren: Beyond Journey's End": "frieren beyond journey s end",
		"Spy x Family 2nd Season":       "spy x family season 2",
		"SPY×FAMILY S2":                 "spy family season 2",
	}
	for input, want := range tests {
		if got := NormalizeTitle(input); got != want {
			t.Errorf("NormalizeTitle(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestEpisodeValues(t *testing.T) {
	got, err := episodeValues(json.RawMessage(`{"sub":["2",1,"1.5"],"dub":[1]}`), "sub")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"2", "1", "1.5"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("episodeValues() = %#v, want %#v", got, want)
	}
}

// graphQLServer answers the metadata queries the client still makes natively.
func graphQLServer(t *testing.T, response string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("x-build-id"); got != buildID {
			t.Errorf("x-build-id = %q, want %q", got, buildID)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", got)
		}
		_, _ = io.WriteString(w, response)
	}))
	t.Cleanup(server.Close)
	return server
}

func TestSearchReadsMetadataNatively(t *testing.T) {
	server := graphQLServer(t, `{"data":{"shows":{"edges":[
		{"_id":"abc","name":"Sousou no Frieren","englishName":"Frieren","aniListId":"154587"}]}}}`)
	client := NewClientWithEndpoints(server.Client(), server.URL, "https://web.test")

	got, err := client.Search(t.Context(), "frieren", "sub")
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	want := []Anime{{ID: "abc", Name: "Sousou no Frieren", English: "Frieren", AniListID: 154587}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Search() = %#v, want %#v", got, want)
	}
}

func TestEpisodesReadsMetadataNatively(t *testing.T) {
	server := graphQLServer(t, `{"data":{"show":{"_id":"abc","availableEpisodesDetail":{"sub":["3","1","2","special"]}}}}`)
	client := NewClientWithEndpoints(server.Client(), server.URL, "https://web.test")

	got, err := client.Episodes(t.Context(), Anime{ID: "abc"}, "sub")
	if err != nil {
		t.Fatalf("Episodes() error = %v", err)
	}
	want := []Episode{
		{ShowID: "abc", Number: 1, Value: "1"},
		{ShowID: "abc", Number: 2, Value: "2"},
		{ShowID: "abc", Number: 3, Value: "3"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Episodes() = %#v, want %#v", got, want)
	}
}

func TestGraphQLSurfacesProviderErrors(t *testing.T) {
	server := graphQLServer(t, `{"errors":[{"message":"show not found"}]}`)
	client := NewClientWithEndpoints(server.Client(), server.URL, "https://web.test")

	_, err := client.Episodes(t.Context(), Anime{ID: "missing"}, "sub")
	if err == nil {
		t.Fatal("Episodes() error = nil, want the provider error to surface")
	}
}

func TestStreamsRequireABrowserSession(t *testing.T) {
	client := NewClientWithEndpoints(nil, "https://api.test", "https://web.test")
	_, err := client.Streams(t.Context(), testEpisode(), "sub", "best")
	if !errors.Is(err, ErrBrowserRequired) {
		t.Fatalf("Streams() error = %v, want ErrBrowserRequired", err)
	}
}

func TestStreamsWrapBrowserFailures(t *testing.T) {
	client := NewClientWithEndpoints(nil, "https://api.test", "https://web.test")
	client.UseBrowser(captureFunc(func(context.Context, browser.Request) ([]browser.Candidate, error) {
		return nil, browser.ErrChallenged
	}))

	_, err := client.Streams(t.Context(), testEpisode(), "sub", "best")
	if !errors.Is(err, browser.ErrChallenged) {
		t.Fatalf("Streams() error = %v, want the browser failure to be preserved", err)
	}
}

func TestClientSatisfiesProviderContract(t *testing.T) {
	// Compile-time check that the browser-only client still fits the
	// provider-neutral interface the service depends on.
	var _ interface {
		Match(context.Context, int, []string, string) (Anime, error)
		Episodes(context.Context, Anime, string) ([]Episode, error)
		Streams(context.Context, Episode, string, string) ([]Stream, error)
	} = NewClient(nil)
}
