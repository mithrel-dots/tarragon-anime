package animeflix

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"tarragon-anime/internal/provider"
)

func TestVerifiedSeasonSurvivesCachedMapping(t *testing.T) {
	const slug = "that-time-i-got-reincarnated-as-a-slime"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/info/"+slug {
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		_, _ = fmt.Fprintf(w, `{"success":true,"data":{"infoX":[{"name":"That Time I Got Reincarnated as a Slime"}],"episodetown":[
			{"season":"1","episode":"5","epId":"%s-1x5"},
			{"season":"3","episode":"5","epId":"%s-3x5"},
			{"season":"4","episode":"5","epId":"%s-4x5"},
			{"season":"4","episode":"1","epId":"%s-4x1"}]}}`, slug, slug, slug, slug)
	}))
	defer server.Close()
	client := NewClientWithEndpoints(server.Client(), server.URL, "")
	match, err := client.Match(t.Context(), 182205, []string{"Tensei Shitara Slime Datta Ken 4th Season"}, "sub")
	if err != nil {
		t.Fatal(err)
	}
	if match.ID != slug+"#season=4" || match.AniListID != 182205 {
		t.Fatalf("match=%+v", match)
	}
	// The store restores only ID and AniListID, not the matched title.
	cached := provider.Anime{ID: match.ID, AniListID: 182205}
	episodes, err := client.Episodes(t.Context(), cached, "sub")
	want := []provider.Episode{
		{ShowID: cached.ID, Number: 1, Value: slug + "-4x1"},
		{ShowID: cached.ID, Number: 5, Value: slug + "-4x5"},
	}
	if err != nil || !reflect.DeepEqual(episodes, want) {
		t.Fatalf("episodes=%+v err=%v", episodes, err)
	}
	if _, err := client.Episodes(t.Context(), provider.Anime{ID: slug}, "sub"); err == nil {
		t.Fatal("base catalogue must still reject season-reset numbering")
	}
	if _, err := client.Episodes(t.Context(), provider.Anime{ID: cached.ID, AniListID: 156822}, "sub"); err == nil {
		t.Fatal("season mapping accepted for a different AniList release")
	}
	if _, err := client.Episodes(t.Context(), provider.Anime{ID: slug + "#season=3"}, "sub"); err == nil {
		t.Fatal("unverified season selector accepted")
	}
}

func TestVerifiedSeasonFailsClosedWhenCatalogueChanges(t *testing.T) {
	for _, test := range []struct{ name, title, season, epID, want string }{
		{"wrong title", "A Different Show", "4", "that-time-i-got-reincarnated-as-a-slime-4x5", "catalogue identity changed"},
		{"missing season", "That Time I Got Reincarnated as a Slime", "3", "that-time-i-got-reincarnated-as-a-slime-3x5", "no valid episodes"},
		{"wrong episode id", "That Time I Got Reincarnated as a Slime", "4", "that-time-i-got-reincarnated-as-a-slime-1x5", "episode identity changed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{
					"infoX":       []map[string]string{{"name": test.title}},
					"episodetown": []map[string]string{{"season": test.season, "episode": "5", "epId": test.epID}},
				}})
			}))
			defer server.Close()
			client := NewClientWithEndpoints(server.Client(), server.URL, "")
			if _, err := client.Match(t.Context(), 182205, nil, "sub"); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("expected %q, got %v", test.want, err)
			}
		})
	}
}
