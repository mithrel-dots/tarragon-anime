package animepahe

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

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
<button data-src="https://kwik.si/e/dub-720" data-resolution="720" data-audio="eng">
<button data-src="https://example.test/e/not-a-stream" data-resolution="2160" data-audio="jpn">`
	if got := parsePlayerLinks(html, "sub", "best"); !reflect.DeepEqual(got, []string{
		"https://example.test/e/not-a-stream", "https://kwik.cx/e/sub-1080", "https://kwik.cx/e/sub-480",
	}) {
		t.Fatalf("sub links = %#v", got)
	}
	if got := parsePlayerLinks(html, "dub", "best"); !reflect.DeepEqual(got, []string{"https://kwik.si/e/dub-720"}) {
		t.Fatalf("dub links = %#v", got)
	}
	if got := parsePlayerLinks(html, "sub", "worst"); !reflect.DeepEqual(got, []string{
		"https://kwik.cx/e/sub-480", "https://kwik.cx/e/sub-1080", "https://example.test/e/not-a-stream",
	}) {
		t.Fatalf("worst sub links = %#v", got)
	}
	for _, quality := range []string{"720p", "240"} {
		if got := parsePlayerLinks(html, "sub", quality); got[0] != "https://kwik.cx/e/sub-480" {
			t.Fatalf("%s links = %#v, want 480p first", quality, got)
		}
	}
}

func TestGetChallengeUsesSolverCookiesAndContext(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if _, err := r.Cookie("clearance"); err != nil {
			w.Header().Set("Server", "cloudflare")
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, "Just a moment...")
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer server.Close()

	client := NewClientWithBaseURL(server.Client(), server.URL)
	called := false
	client.solver = func(ctx context.Context, origin string) ([]*http.Cookie, error) {
		called = true
		if origin != server.URL {
			t.Fatalf("solver origin = %q, want %q", origin, server.URL)
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("solver context has no deadline")
		}
		return []*http.Cookie{{Name: "clearance", Value: "yes", Path: "/"}}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	body, err := client.get(ctx, server.URL+"/api", false)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "ok" || !called || requests != 2 {
		t.Fatalf("body=%q solver=%v requests=%d", body, called, requests)
	}
}

func TestGetChallengeRejectsEmptySolverCookies(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "cloudflare")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()
	client := NewClientWithBaseURL(server.Client(), server.URL)
	client.solver = func(context.Context, string) ([]*http.Cookie, error) { return nil, nil }
	_, err := client.get(context.Background(), server.URL, true)
	if err == nil || !strings.Contains(err.Error(), "no clearance cookies") {
		t.Fatalf("get() error = %v", err)
	}
}

func TestDisabledBrowserNeverSolvesChallenge(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ok" {
			_, _ = io.WriteString(w, "ok")
			return
		}
		w.Header().Set("Server", "cloudflare")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()
	client := NewClientWithBaseURL(server.Client(), server.URL).WithBrowser(false)
	client.solver = func(context.Context, string) ([]*http.Cookie, error) {
		t.Fatal("disabled browser launched the challenge solver")
		return nil, nil
	}
	if _, err := client.get(t.Context(), server.URL, true); err == nil || !strings.Contains(err.Error(), "requires browser clearance") {
		t.Fatalf("expected challenge error, got %v", err)
	}
	if body, err := client.get(t.Context(), server.URL+"/ok", true); err != nil || string(body) != "ok" {
		t.Fatalf("HTTP-only request failed: %q, %v", body, err)
	}
}

func TestUnpackPacker(t *testing.T) {
	got := unpackPacker("0", 62, 1, []string{"https://video.test/stream.m3u8"})
	if !strings.Contains(got, "https://video.test/stream.m3u8") {
		t.Fatalf("unpackPacker() = %q", got)
	}
}

func TestUnpackPackerRejectsInvalidRadix(t *testing.T) {
	for _, base := range []int{1, 63, 1000} {
		if got := unpackPacker("0", base, 1, []string{"unused"}); got != "" {
			t.Fatalf("unpackPacker(base=%d) = %q, want empty", base, got)
		}
	}
	if got := unpackPacker("0", 62, 2, []string{"only-one"}); got != "" {
		t.Fatalf("unpackPacker(count beyond words) = %q, want empty", got)
	}
}

func TestExtractKwikPackerAllowsMultilineAndRejectsHostileRadix(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "eval(function(p,a,c,k,e,d){\nreturn p\n}\n('0',63,1,'https://bad.test/stream.m3u8'.split('|'),0,{}))")
	}))
	defer server.Close()
	client := NewClientWithBaseURL(server.Client(), server.URL)
	if _, err := client.extractKwik(context.Background(), server.URL); err == nil {
		t.Fatal("extractKwik() accepted a packed script with radix 63")
	}
}

func TestKwikURLValidationAndOrigin(t *testing.T) {
	for _, test := range []struct {
		raw  string
		good bool
	}{
		{"https://kwik.si/e/abc", true},
		{"https://cdn.kwik.cx/e/abc", true},
		{"http://kwik.cx/e/abc", false},
		{"https://kwik.cx/not-embed/abc", false},
		{"https://kwik.cx.evil.test/e/abc", false},
		{"javascript://kwik.cx/e/abc", false},
	} {
		if got := isKwikURL(test.raw); got != test.good {
			t.Errorf("isKwikURL(%q) = %v, want %v", test.raw, got, test.good)
		}
	}
	if got := kwikOrigin("https://kwik.si/e/abc"); got != "https://kwik.si/" {
		t.Fatalf("kwikOrigin() = %q", got)
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

func TestEpisodesRejectsConflictsAndSkipsFractionalSpecials(t *testing.T) {
	for _, test := range []struct {
		name      string
		data      string
		wantError bool
	}{
		{"duplicate", `[{"episode":1,"session":"one"},{"episode":"1","session":"one"},{"episode":1.5,"session":"special"},{"episode":2,"session":""}]`, false},
		{"conflict", `[{"episode":1,"session":"one"},{"episode":1,"session":"another"}]`, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, `{"current_page":1,"last_page":1,"data":`+test.data+`}`)
			}))
			defer server.Close()
			episodes, err := NewClientWithBaseURL(server.Client(), server.URL).Episodes(t.Context(), provider.Anime{ID: "1:session"}, "sub")
			if test.wantError {
				if err == nil || !strings.Contains(err.Error(), "conflicting session") {
					t.Fatalf("expected conflict, got %v", err)
				}
			} else if err != nil || len(episodes) != 1 || episodes[0].Value != "one" {
				t.Fatalf("episodes=%v err=%v", episodes, err)
			}
		})
	}
}
