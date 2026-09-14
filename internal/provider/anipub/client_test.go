package anipub

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"tarragon-anime/internal/browser"
)

// captureFunc adapts a function to the Resolver interface so stream resolution
// can be tested without Chromium.
type captureFunc func(context.Context, browser.Request) ([]browser.Candidate, error)

func (f captureFunc) Capture(ctx context.Context, req browser.Request) ([]browser.Candidate, error) {
	return f(ctx, req)
}

// catalogueServer serves the two metadata endpoints the provider depends on.
func catalogueServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/search/"):
			_, _ = w.Write([]byte(`[
				{"Name":"Test Show Season 4","Id":8347,"finder":"test-show-season-4"},
				{"Name":"Test Show","Id":146,"finder":"test-show"},
				{"Name":"Test Show: Specials","Id":2185,"finder":"test-show-specials"}
			]`))
		case strings.HasPrefix(r.URL.Path, "/v1/api/details/8347"):
			// The first episode sits beside the list rather than inside it.
			_, _ = w.Write([]byte(`{"local":{"name":"Episode 1","finder":"test-show-season-4",
				"link":"src=https://anipub.test/play/59970/1/sub","ep":[
					{"link":"src=https://anipub.test/play/59970/2/sub"},
					{"link":"src=https://anipub.test/play/59970/3/sub"}
				]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// Seasons, specials and recap cuts sit in the catalogue under names a word
// apart, so only an exact title may match: a near miss plays the wrong show
// rather than failing where anyone can see it.
func TestMatchRequiresAnExactTitle(t *testing.T) {
	server := catalogueServer(t)
	client := NewClientWithBaseURL(server.Client(), server.URL)

	anime, err := client.Match(context.Background(), 182205, []string{"Test Show Season 4"}, "sub")
	if err != nil {
		t.Fatalf("Match() error = %v", err)
	}
	if anime.ID != "8347" || anime.AniListID != 182205 {
		t.Fatalf("Match() = %+v, want the season 4 entry", anime)
	}

	if _, err := client.Match(context.Background(), 182205, []string{"Test Show Season 9"}, "sub"); err == nil {
		t.Fatal("Match() accepted a title the catalogue does not carry")
	}
}

// Punctuation and casing differ between the tracker and the catalogue, and an
// otherwise exact title must still match across them.
func TestMatchNormalizesPunctuation(t *testing.T) {
	server := catalogueServer(t)
	client := NewClientWithBaseURL(server.Client(), server.URL)

	anime, err := client.Match(context.Background(), 1, []string{"test show: specials"}, "sub")
	if err != nil {
		t.Fatalf("Match() error = %v", err)
	}
	if anime.ID != "2185" {
		t.Fatalf("Match() = %q, want the specials entry", anime.ID)
	}
}

// The details response keeps episode one beside the list instead of in it, so
// reading only the list silently loses the first episode.
func TestEpisodesIncludeTheOneOutsideTheList(t *testing.T) {
	server := catalogueServer(t)
	client := NewClientWithBaseURL(server.Client(), server.URL)

	episodes, err := client.Episodes(context.Background(), Anime{ID: "8347"}, "sub")
	if err != nil {
		t.Fatalf("Episodes() error = %v", err)
	}
	want := []Episode{
		{ShowID: "59970", Number: 1, Value: "1"},
		{ShowID: "59970", Number: 2, Value: "2"},
		{ShowID: "59970", Number: 3, Value: "3"},
	}
	if !reflect.DeepEqual(episodes, want) {
		t.Fatalf("Episodes() = %#v, want %#v", episodes, want)
	}
}

// The endpoint answers with a different JSON shape per result count, and an
// exact title arrives as a bare object rather than an array. Decoding only the
// array form fails on precisely the case a conservative match depends on.
func TestDecodeSearchResultsHandlesEveryShape(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
		want []searchResult
	}{
		{
			name: "several matches",
			body: `[{"Name":"A","Id":1,"finder":"a"},{"Name":"B","Id":2,"finder":"b"}]`,
			want: []searchResult{{Name: "A", ID: 1, Finder: "a"}, {Name: "B", ID: 2, Finder: "b"}},
		},
		{
			name: "exactly one match",
			body: `{"Name":"A","Id":1,"finder":"a"}`,
			want: []searchResult{{Name: "A", ID: 1, Finder: "a"}},
		},
		{"no match", `{"found":false}`, nil},
		{"empty body", ``, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := decodeSearchResults([]byte(test.body))
			if err != nil {
				t.Fatalf("decodeSearchResults() error = %v", err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("decodeSearchResults() = %#v, want %#v", got, test.want)
			}
		})
	}
}

// An exact title is the single-object case end to end, so Match must resolve
// it rather than fail on the payload shape.
func TestMatchResolvesASingleObjectResult(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/api/search/") {
			_, _ = w.Write([]byte(`{"Name":"Only Match","Id":4242,"finder":"only-match"}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)

	client := NewClientWithBaseURL(server.Client(), server.URL)
	anime, err := client.Match(context.Background(), 7, []string{"Only Match"}, "sub")
	if err != nil {
		t.Fatalf("Match() error = %v", err)
	}
	if anime.ID != "4242" {
		t.Fatalf("Match() = %q, want the single result", anime.ID)
	}
}

func TestWatchURL(t *testing.T) {
	got := watchURL("https://anipub.test/", "59970", "12", "DUB")
	want := "https://anipub.test/play/59970/12/dub"
	if got != want {
		t.Fatalf("watchURL() = %q, want %q", got, want)
	}
}

// The player encrypts its source response, so without a browser there is no
// HTTP-only path to a playable URL and the caller must be told plainly.
func TestStreamsWithoutABrowserIsReported(t *testing.T) {
	server := catalogueServer(t)
	client := NewClientWithBaseURL(server.Client(), server.URL)

	_, err := client.Streams(context.Background(), Episode{ShowID: "59970", Number: 1, Value: "1"}, "sub", "best")
	if !errors.Is(err, ErrBrowserRequired) {
		t.Fatalf("Streams() error = %v, want ErrBrowserRequired", err)
	}
}

// The master playlist lists every rendition, so handing it over lets the player
// choose the quality. Preferring a variant would pin playback to whichever
// rendition the page happened to request first.
func TestStreamsPreferTheMasterPlaylist(t *testing.T) {
	server := catalogueServer(t)
	client := NewClientWithBaseURL(server.Client(), server.URL)

	master := "https://cdn.test/anime/abc/def/master.m3u8"
	subtitle := "https://subs.test/anime/abc/def/subtitles/eng.vtt"
	var captured string
	client.UseBrowser(captureFunc(func(_ context.Context, req browser.Request) ([]browser.Candidate, error) {
		captured = req.PageURL
		return []browser.Candidate{
			{URL: "https://cdn.test/anime/abc/def/index-f2-v1-a1.m3u8", Status: 200, Observed: time.Second},
			{URL: subtitle, Status: 200, MIME: "text/vtt", Observed: 1200 * time.Millisecond},
			{URL: master, Status: 200, Observed: 1500 * time.Millisecond},
		}, nil
	}))

	streams, err := client.Streams(context.Background(), Episode{ShowID: "59970", Number: 1, Value: "1"}, "sub", "best")
	if err != nil {
		t.Fatalf("Streams() error = %v", err)
	}
	if captured != server.URL+"/play/59970/1/sub" {
		t.Fatalf("captured page = %q, want the episode's play route", captured)
	}
	if streams[0].URL != master {
		t.Fatalf("streams[0].URL = %q, want the master playlist %q", streams[0].URL, master)
	}
	if streams[0].Subtitle != subtitle {
		t.Fatalf("streams[0].Subtitle = %q, want the captured subtitle %q", streams[0].Subtitle, subtitle)
	}
}

// A capture costs a page load and the URLs are signed, so a repeat must be
// served from the cache and dropped again when playback reports a failure.
func TestStreamsAreCachedUntilInvalidated(t *testing.T) {
	server := catalogueServer(t)
	client := NewClientWithBaseURL(server.Client(), server.URL)

	captures := 0
	client.UseBrowser(captureFunc(func(context.Context, browser.Request) ([]browser.Candidate, error) {
		captures++
		return []browser.Candidate{{URL: "https://cdn.test/a/master.m3u8", Status: 200}}, nil
	}))

	episode := Episode{ShowID: "59970", Number: 1, Value: "1"}
	for range 2 {
		if _, err := client.Streams(context.Background(), episode, "sub", "best"); err != nil {
			t.Fatalf("Streams() error = %v", err)
		}
	}
	if captures != 1 {
		t.Fatalf("captures = %d, want the second resolution served from cache", captures)
	}
	client.InvalidateStreams(episode, "sub", "best")
	if _, err := client.Streams(context.Background(), episode, "sub", "best"); err != nil {
		t.Fatalf("Streams() error = %v", err)
	}
	if captures != 2 {
		t.Fatalf("captures = %d, want a fresh capture after invalidation", captures)
	}
}

func TestClassifyRejectsWhatIsNotPlayable(t *testing.T) {
	for _, test := range []struct {
		name      string
		candidate browser.Candidate
		want      mediaClass
	}{
		{"master playlist", browser.Candidate{URL: "https://cdn.test/a/master.m3u8", Status: 200}, mediaPlaylist},
		{"playlist by mime", browser.Candidate{URL: "https://cdn.test/a/stream", Status: 200, MIME: "application/x-mpegurl"}, mediaPlaylist},
		{"subtitle", browser.Candidate{URL: "https://subs.test/a/eng.vtt", Status: 200}, mediaSubtitle},
		// A chunk plays for a few seconds on its own and is never the episode.
		{"hls segment", browser.Candidate{URL: "https://cdn.test/a/seg-12.ts", Status: 200}, mediaNone},
		{"dash chunk", browser.Candidate{URL: "https://cdn.test/a/chunk-3.m4s", Status: 200}, mediaNone},
		{"blob url", browser.Candidate{URL: "blob:https://anipub.test/9f92", Status: 200}, mediaNone},
		{"ad media", browser.Candidate{URL: "https://s0.2mdn.net/spot.m3u8", Status: 200}, mediaNone},
		{"failed response", browser.Candidate{URL: "https://cdn.test/a/master.m3u8", Status: 403}, mediaNone},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := classify(test.candidate); got != test.want {
				t.Fatalf("classify(%q) = %v, want %v", test.candidate.URL, got, test.want)
			}
		})
	}
}

// A signed CDN validates the referrer against the one it issued the token for,
// so a header invented on the player's behalf is rejected outright.
func TestPlaybackHeadersNeverInventAReferer(t *testing.T) {
	headers := playbackHeaders(browser.Candidate{URL: "https://cdn.test/a/master.m3u8"})
	if _, ok := headers["Referer"]; ok {
		t.Fatalf("Referer = %q, want none when the browser sent none", headers["Referer"])
	}
	if headers["User-Agent"] != browser.UserAgent {
		t.Fatalf("User-Agent = %q, want the browser user agent", headers["User-Agent"])
	}

	observed := playbackHeaders(browser.Candidate{
		URL:     "https://cdn.test/a/master.m3u8",
		Headers: map[string]string{"Referer": "https://megaplay.test/", "Origin": "https://megaplay.test"},
	})
	if observed["Referer"] != "https://megaplay.test/" {
		t.Fatalf("Referer = %q, want the referer the browser sent", observed["Referer"])
	}
}
