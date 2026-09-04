package animepahe

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"tarragon-anime/internal/provider"
)

func TestMatchAndEpisodes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("m") == "search" {
			_, _ = io.WriteString(w, `{"data":[{"id":4,"title":"Frieren: Beyond Journey's End","episodes":28,"session":"session-id"}]}`)
			return
		}
		if r.URL.Query().Get("m") == "release" {
			_, _ = io.WriteString(w, `{"current_page":1,"last_page":1,"data":[{"episode":1,"session":"episode-1"},{"episode":2,"session":"episode-2"}]}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	client := NewClientWithBaseURL(server.Client(), server.URL)
	match, err := client.Match(context.Background(), 154587, []string{"Frieren: Beyond Journey's End"}, "sub")
	if err != nil {
		t.Fatal(err)
	}
	if match.ID != "4:session-id" {
		t.Fatalf("match ID = %q", match.ID)
	}
	episodes, err := client.Episodes(context.Background(), match, "sub")
	if err != nil {
		t.Fatal(err)
	}
	want := []provider.Episode{
		{ShowID: "4:session-id", Number: 1, Value: "episode-1"},
		{ShowID: "4:session-id", Number: 2, Value: "episode-2"},
	}
	if !reflect.DeepEqual(episodes, want) {
		t.Fatalf("Episodes() = %#v, want %#v", episodes, want)
	}
}

func TestParsePlayerLinks(t *testing.T) {
	html := `<button data-src="https://kwik.cx/e/sub-480" data-resolution="480" data-audio="jpn">
<button data-src="https://kwik.cx/e/sub-1080" data-resolution="1080" data-audio="jpn">
<button data-src="https://kwik.cx/e/dub-720" data-resolution="720" data-audio="eng">`
	if got := parsePlayerLinks(html, "sub"); !reflect.DeepEqual(got, []string{
		"https://kwik.cx/e/sub-1080", "https://kwik.cx/e/sub-480",
	}) {
		t.Fatalf("sub links = %#v", got)
	}
	if got := parsePlayerLinks(html, "dub"); !reflect.DeepEqual(got, []string{"https://kwik.cx/e/dub-720"}) {
		t.Fatalf("dub links = %#v", got)
	}
}

func TestUnpackPacker(t *testing.T) {
	got := unpackPacker("0", 62, 1, []string{"https://video.test/stream.m3u8"})
	if !strings.Contains(got, "https://video.test/stream.m3u8") {
		t.Fatalf("unpackPacker() = %q", got)
	}
}

func TestExtractKwikM3U8(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `eval(function(p,a,c,k,e,d){return p}('0',62,1,'https://video.test/stream.m3u8'.split('|'),0,{}))`)
	}))
	defer server.Close()

	client := NewClientWithBaseURL(server.Client(), server.URL)
	got, err := client.extractKwik(context.Background(), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://video.test/stream.m3u8" {
		t.Fatalf("extractKwik() = %q", got)
	}
}

func TestIsChallengeRecognizesCloudflare(t *testing.T) {
	resp := &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{"Server": {"cloudflare"}}}
	if !isChallenge(resp, []byte(`<title>Just a moment...</title><script src="/cdn-cgi/challenge-platform/x"></script>`)) {
		t.Fatal("isChallenge() did not recognize a Cloudflare challenge")
	}
}
