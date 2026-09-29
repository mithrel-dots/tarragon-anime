package anineko

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestMatchEpisodesAndBibiembStreams(t *testing.T) {
	const subtitle = "https://subs.test/episode-1.vtt"
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("User-Agent"); got != userAgent {
			t.Errorf("User-Agent = %q, want %q", got, userAgent)
		}
		if got := r.Header.Get("Referer"); got == "" {
			t.Error("Referer was not sent")
		}
		switch r.URL.Path {
		case "/ajax/search":
			_, _ = io.WriteString(w, `{"success":true,"results":[{"title":"Test Show Season 2","url":"/watch/test-show-season-2","image":"/img/test.jpg","meta":"TV"}]}`)
		case "/watch/test-show-season-2":
			_, _ = io.WriteString(w, `<a href="/watch/test-show-season-2/ep-12">12</a><a href="/watch/test-show-season-2/ep-1">1</a><a href="/watch/test-show-season-2/ep-1">1</a>`)
		case "/watch/test-show-season-2/ep-1":
			encoded := url.QueryEscape(subtitle)
			_, _ = io.WriteString(w, `<div data-id="sub"><div data-video="https://bibiemb.test/embed/bibi?sub=`+encoded+`"></div></div>`)
		case "/embed/bibi":
			_, _ = io.WriteString(w, `const src = "`+server.URL+`/media/master.m3u8"`)
		case "/media/master.m3u8":
			_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1,NAME=\"720p\"\n720.m3u8\n#EXT-X-STREAM-INF:BANDWIDTH=2,NAME=\"1080p\"\n1080.m3u8\n")
		case "/media/720.m3u8", "/media/1080.m3u8":
			_, _ = io.WriteString(w, "#EXTM3U\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewClientWithBaseURL(&http.Client{Transport: rewriteHostTransport{target: server.URL}}, server.URL)
	show, err := client.Match(t.Context(), 123, []string{"Test Show Season 2"}, "sub")
	if err != nil {
		t.Fatalf("Match() error = %v", err)
	}
	if show.ID != "test-show-season-2" || show.AniListID != 123 {
		t.Fatalf("Match() = %+v", show)
	}

	episodes, err := client.Episodes(t.Context(), show, "sub")
	if err != nil {
		t.Fatalf("Episodes() error = %v", err)
	}
	if len(episodes) != 2 || episodes[0].Number != 1 || episodes[1].Number != 12 {
		t.Fatalf("Episodes() = %#v, want episodes 1 and 12", episodes)
	}

	streams, err := client.Streams(t.Context(), episodes[0], "sub", "best")
	if err != nil {
		t.Fatalf("Streams() error = %v", err)
	}
	if len(streams) != 1 || !strings.HasSuffix(streams[0].URL, "/media/1080.m3u8") {
		t.Fatalf("Streams() = %#v, want the 1080p variant", streams)
	}
	if streams[0].Headers["Referer"] != "https://bibiemb.test/embed/bibi?sub="+url.QueryEscape(subtitle) {
		t.Fatalf("stream referer = %q", streams[0].Headers["Referer"])
	}
	if streams[0].Subtitle != subtitle {
		t.Fatalf("stream subtitle = %q, want %q", streams[0].Subtitle, subtitle)
	}

	worst, err := client.Streams(t.Context(), episodes[0], "sub", "worst")
	if err != nil {
		t.Fatalf("Streams(worst) error = %v", err)
	}
	if !strings.HasSuffix(worst[0].URL, "/media/720.m3u8") {
		t.Fatalf("Streams(worst) = %#v, want the 720p variant", worst)
	}
}

func TestMatchTriesTheNextAliasAfterAnEmptySearch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("q") == "missing alias" {
			_, _ = io.WriteString(w, `{"success":true,"results":[]}`)
			return
		}
		_, _ = io.WriteString(w, `{"success":true,"results":[{"title":"Found Show","url":"/watch/found-show"}]}`)
	}))
	defer server.Close()

	client := NewClientWithBaseURL(server.Client(), server.URL)
	got, err := client.Match(t.Context(), 456, []string{"missing alias", "Found Show"}, "sub")
	if err != nil {
		t.Fatalf("Match() error = %v", err)
	}
	if got.ID != "found-show" || got.AniListID != 456 {
		t.Fatalf("Match() = %+v", got)
	}
}

type rewriteHostTransport struct {
	target string
}

func (t rewriteHostTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Host != "bibiemb.test" {
		return http.DefaultTransport.RoundTrip(request)
	}
	target, err := url.Parse(t.target)
	if err != nil {
		return nil, err
	}
	clone := request.Clone(request.Context())
	clone.URL.Scheme = target.Scheme
	clone.URL.Host = target.Host
	return http.DefaultTransport.RoundTrip(clone)
}

func TestExtractLangEmbedURLsAndSubtitleFallbacks(t *testing.T) {
	page := `<div data-id="sub"><div data-video="https://bibiemb.test/a"></div><div data-video="https://vibeplayer.test/b?sub=https%3A%2F%2Fsubs.test%2Fsoft.vtt"></div></div><div data-id="hsub"><div data-video="https://bibiemb.test/hard"></div></div><div data-id="dub"><div data-video="https://vibeplayer.test/dub"></div></div>`
	groups := extractLangEmbedURLs(page)
	if len(groups["sub"]) != 2 || len(groups["hsub"]) != 1 || len(groups["dub"]) != 1 {
		t.Fatalf("groups = %#v", groups)
	}
	if got := resolveEmbedHost(groups["sub"][1]); got != "vibeplayer" {
		t.Fatalf("resolveEmbedHost() = %q, want vibeplayer", got)
	}
	if got := resolveSubtitle(groups["sub"][1], `const subtitle = "https://subs.test/page.vtt"`); got != "https://subs.test/soft.vtt" {
		t.Fatalf("resolveSubtitle() = %q", got)
	}
	if got := resolveSubtitle("https://bibiemb.test/a", `const subtitle = "https://subs.test/page.vtt"`); got != "https://subs.test/page.vtt" {
		t.Fatalf("resolveSubtitle() fallback = %q", got)
	}
}

func TestChooseVariantUsesResolutionAndQuality(t *testing.T) {
	variants := parseVariants("https://cdn.test/path/master.m3u8", "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=100,RESOLUTION=1280x720\n720.m3u8\n#EXT-X-STREAM-INF:BANDWIDTH=200,RESOLUTION=1920x1080\n1080.m3u8\n")
	if got := chooseVariant(variants, "best").URL; !strings.HasSuffix(got, "/1080.m3u8") {
		t.Fatalf("best variant = %q", got)
	}
	if got := chooseVariant(variants, "worst").URL; !strings.HasSuffix(got, "/720.m3u8") {
		t.Fatalf("worst variant = %q", got)
	}
	for _, quality := range []string{"720p", "900", "480p"} {
		if got := chooseVariant(variants, quality).URL; !strings.HasSuffix(got, "/720.m3u8") {
			t.Fatalf("%s variant = %q, want 720p", quality, got)
		}
	}
}

func TestStreamsTryHardSubsAfterSoftSubsFail(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/watch/show/ep-1":
			io.WriteString(w, `<div data-id="sub"><div data-video="https://bibiemb.test/soft"></div></div><div data-id="hsub"><div data-video="https://bibiemb.test/hard"></div></div><div data-id="dub"><div data-video="https://bibiemb.test/wrong-language"></div></div>`)
		case "/soft":
			http.Error(w, "source unavailable", http.StatusServiceUnavailable)
		case "/hard":
			io.WriteString(w, `const src = "`+server.URL+`/master.m3u8";`)
		case "/master.m3u8":
			io.WriteString(w, "#EXTM3U\n#EXT-X-STREAM-INF:RESOLUTION=1280x720\n720.m3u8\n")
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c := NewClientWithBaseURL(&http.Client{Transport: rewriteHostTransport{target: server.URL}}, server.URL)
	defer c.Close()
	streams, err := c.Streams(t.Context(), Episode{ShowID: "show", Number: 1}, "sub", "best")
	if err != nil || len(streams) != 1 || streams[0].Headers["Referer"] != "https://bibiemb.test/hard" {
		t.Fatalf("hard sub fallback: %v, %v", streams, err)
	}
}

func TestStripPNGWrapper(t *testing.T) {
	prefix, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAA")
	payload := []byte{0x47, 0x01, 0x02, 0x03}
	body := append(append(prefix, pngIENDMarker...), payload...)
	if got := stripPNGWrapper(body); string(got) != string(payload) {
		t.Fatalf("stripPNGWrapper() = %v, want %v", got, payload)
	}
}

func TestVibeProxyRewritesPlaylistAndStripsSegmentWrapper(t *testing.T) {
	payload := []byte{0x47, 0x11, 0x22, 0x33}
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Referer") != server.URL+"/embed/vibe" {
			t.Errorf("remote Referer = %q", r.Header.Get("Referer"))
		}
		switch r.URL.Path {
		case "/media/master.m3u8":
			_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=10\nvariant.m3u8\n")
		case "/media/variant.m3u8":
			_, _ = io.WriteString(w, "#EXTM3U\n#EXTINF:4,\nsegment.ts\n")
		case "/media/segment.ts":
			_, _ = w.Write(append(append([]byte("PNG wrapper"), pngIENDMarker...), payload...))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewClientWithBaseURL(server.Client(), server.URL)
	defer client.Close()
	proxy, err := client.vibeProxy(t.Context())
	if err != nil {
		t.Fatalf("vibeProxy() error = %v", err)
	}
	masterURL, err := proxy.register(t.Context(), server.URL+"/media/master.m3u8", server.URL+"/embed/vibe")
	if err != nil {
		t.Fatalf("register() error = %v", err)
	}

	master, err := http.Get(masterURL)
	if err != nil {
		t.Fatalf("proxy master request: %v", err)
	}
	masterBody, _ := io.ReadAll(master.Body)
	_ = master.Body.Close()
	masterFields := strings.Fields(string(masterBody))
	variantURL := masterFields[len(masterFields)-1]
	variant, err := http.Get(variantURL)
	if err != nil {
		t.Fatalf("proxy variant request: %v", err)
	}
	variantBody, _ := io.ReadAll(variant.Body)
	_ = variant.Body.Close()
	variantFields := strings.Fields(string(variantBody))
	segmentURL := variantFields[len(variantFields)-1]
	segment, err := http.Get(segmentURL)
	if err != nil {
		t.Fatalf("proxy segment request: %v", err)
	}
	segmentBody, _ := io.ReadAll(segment.Body)
	_ = segment.Body.Close()
	if string(segmentBody) != string(payload) {
		t.Fatalf("proxied segment = %v, want %v", segmentBody, payload)
	}
}

func TestClientSatisfiesProviderContract(t *testing.T) {
	var _ interface {
		Match(context.Context, int, []string, string) (Anime, error)
		Episodes(context.Context, Anime, string) ([]Episode, error)
		Streams(context.Context, Episode, string, string) ([]Stream, error)
	} = NewClient(nil)
}
