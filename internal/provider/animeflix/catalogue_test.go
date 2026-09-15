package animeflix

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"tarragon-anime/internal/provider"
)

func TestMatchPaginatesEscapesAndValidatesInfo(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.EscapedPath())
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.EscapedPath() {
		case "/search/My%20Show%2FPart/1":
			w.Write([]byte(`{"success":true,"data":{"nextpageavailable":true,"animeList":[]}}`))
		case "/search/My%20Show%2FPart/2":
			w.Write([]byte(`{"success":true,"data":{"nextpageavailable":false,"animeList":[{"title":"My Show/Part","id":"slug"}]}}`))
		case "/info/slug":
			w.Write([]byte(`{"success":true,"data":{"infoX":[{"name":"My Show/Part"}],"episodetown":[{"season":"1","episode":"1","epId":"episode-1"}]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	got, err := NewClientWithEndpoints(server.Client(), server.URL, "https://origin.example").Match(context.Background(), 7, []string{"My Show/Part"}, "sub")
	if err != nil || got.ID != "slug" || got.AniListID != 7 {
		t.Fatalf("Match() = %#v, %v", got, err)
	}
	if !strings.Contains(strings.Join(paths, "|"), "/search/My%20Show%2FPart/2") {
		t.Fatalf("paths = %v, want escaped complete pagination", paths)
	}
}

func TestMatchRejectsAmbiguousAndUnvalidatedCandidates(t *testing.T) {
	for name, testCase := range map[string]struct {
		search string
		info   string
		want   string
	}{
		"ambiguous":              {`{"nextpageavailable":false,"animeList":[{"title":"Naruto","id":"a"},{"title":"Naruto","id":"b"}]}`, `{"infoX":[{"name":"Naruto"}]}`, "ambiguous"},
		"missing info":           {`{"nextpageavailable":false,"animeList":[{"title":"Naruto","id":"a"}]}`, `{"infoX":[]}`, "missing info"},
		"empty episodes":         {`{"nextpageavailable":false,"animeList":[{"title":"Naruto","id":"a"}]}`, `{"infoX":[{"name":"Naruto"}],"episodetown":[]}`, "no valid episodes"},
		"reset season collision": {`{"nextpageavailable":false,"animeList":[{"title":"Naruto","id":"a"}]}`, `{"infoX":[{"name":"Naruto"}],"episodetown":[{"season":"1","episode":"1","epId":"s1e1"},{"season":"2","episode":"1","epId":"s2e1"}]}`, "conflicting epIds"},
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if strings.HasPrefix(r.URL.Path, "/search/") {
					w.Write([]byte(`{"success":true,"data":` + testCase.search + `}`))
					return
				}
				w.Write([]byte(`{"success":true,"data":` + testCase.info + `}`))
			}))
			defer server.Close()
			_, err := NewClientWithEndpoints(server.Client(), server.URL, "").Match(context.Background(), 1, []string{"Naruto"}, "sub")
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("error = %v, want %q", err, testCase.want)
			}
		})
	}
}

func TestNormalizeAnimeFlixTitleUsesConservativeWordTokens(t *testing.T) {
	for _, testCase := range []struct {
		left, right string
		equal       bool
	}{
		{"Naruto: Shippuden", "Naruto Shippuden", true},
		{"Naruto   Shippuden", "Naruto-Shippuden", true},
		{"Naruto", "Naruto Shippuden", false},
		{"---", "Naruto", false},
	} {
		if got := normalizeAnimeFlixTitle(testCase.left) == normalizeAnimeFlixTitle(testCase.right); got != testCase.equal {
			t.Errorf("normalize(%q) == normalize(%q): %v, want %v", testCase.left, testCase.right, got, testCase.equal)
		}
	}
	if got := uniqueAnimeFlixTitles([]string{"---", "  ", "Naruto"}); len(got) != 1 || got[0] != "Naruto" {
		t.Fatalf("uniqueAnimeFlixTitles() = %#v, want only Naruto", got)
	}
}

func TestEpisodesDeduplicatesSortsAndRejectsConflictingNumbers(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"success":true,"data":{"infoX":[{"name":"Show"}],"episodetown":[{"season":"1","episode":"2","epId":"two"},{"season":"1","episode":"1","epId":"one"},{"season":"2","episode":"2","epId":"two"},{"season":"special","episode":"x","epId":"special"}]}}`))
	}))
	defer server.Close()
	got, err := NewClientWithEndpoints(server.Client(), server.URL, "").Episodes(context.Background(), provider.Anime{ID: "show"}, "dub")
	if err != nil || len(got) != 2 || got[0].Number != 1 || got[1].Value != "two" {
		t.Fatalf("Episodes() = %#v, %v", got, err)
	}
}

func TestEpisodesRejectsResetSeasonNumberCollision(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"success":true,"data":{"infoX":[{"name":"Show"}],"episodetown":[{"season":"1","episode":"1","epId":"s1e1"},{"season":"2","episode":"1","epId":"s2e1"}]}}`))
	}))
	defer server.Close()
	_, err := NewClientWithEndpoints(server.Client(), server.URL, "").Episodes(context.Background(), provider.Anime{ID: "show"}, "sub")
	if err == nil || !strings.Contains(err.Error(), "conflicting epIds") {
		t.Fatalf("error = %v, want reset-season collision rejection", err)
	}
}

func TestEpisodesRejectsNoValidEpisodes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"success":true,"data":{"infoX":[{"name":"Show"}],"episodetown":[{"season":"special","episode":"0","epId":"special"},{"season":"special","episode":"x","epId":"bad"},{"season":"special","episode":"1","epId":""}]}}`))
	}))
	defer server.Close()
	_, err := NewClientWithEndpoints(server.Client(), server.URL, "").Episodes(context.Background(), provider.Anime{ID: "show"}, "sub")
	if err == nil || !strings.Contains(err.Error(), "no valid episodes") {
		t.Fatalf("error = %v, want no-valid-episodes rejection", err)
	}
}

func TestMatchRetriesPunctuationSensitiveSearch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/search/Naruto: Shippuden/1":
			_, _ = w.Write([]byte(`{"success":true,"data":{"animeList":[]}}`))
		case "/search/naruto shippuden/1":
			_, _ = w.Write([]byte(`{"success":true,"data":{"animeList":[{"title":"Naruto Shippuden","id":"naruto-shippuden"}]}}`))
		case "/info/naruto-shippuden":
			_, _ = w.Write([]byte(`{"success":true,"data":{"infoX":[{"name":"Naruto Shippuden"}],"episodetown":[{"season":"1","episode":"1","epId":"naruto-shippuden-1x1"}]}}`))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := NewClientWithEndpoints(server.Client(), server.URL, "")
	if _, err := client.Match(t.Context(), 1735, []string{"Naruto: Shippuden"}, "sub"); err != nil {
		t.Fatal(err)
	}
}
