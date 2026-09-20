package animeflix

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"tarragon-anime/internal/provider"
	"tarragon-anime/internal/streamlog"
)

type rerouteTransport struct {
	base *url.URL
	mu   sync.Mutex
	seen []requestRecord
}

type requestRecord struct {
	method, url, cookie, referer, requested, contentType, body string
}

func (t *rerouteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	original := req.URL.String()
	var requestBody []byte
	if req.Body != nil {
		requestBody, _ = io.ReadAll(req.Body)
		req.Body = io.NopCloser(bytes.NewReader(requestBody))
		clone.Body = io.NopCloser(bytes.NewReader(requestBody))
	}
	t.mu.Lock()
	t.seen = append(t.seen, requestRecord{method: req.Method, url: original, cookie: req.Header.Get("Cookie"), referer: req.Header.Get("Referer"), requested: req.Header.Get("X-Requested-With"), contentType: req.Header.Get("Content-Type"), body: string(requestBody)})
	t.mu.Unlock()
	clone.Header.Set("X-Test-Original-URL", original)
	clone.URL.Scheme, clone.URL.Host = t.base.Scheme, t.base.Host
	response, err := http.DefaultTransport.RoundTrip(clone)
	if response != nil {
		response.Request = req.Clone(req.Context())
	}
	return response, err
}

func (t *rerouteTransport) records() []requestRecord {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]requestRecord(nil), t.seen...)
}

func TestStreamsResolvesRealMyStreamURLsWithScopedCookiesAndFallback(t *testing.T) {
	const (
		first  = "https://as-cdn1.top/video/0123456789abcdef0123456789abcdef?token=first-secret"
		second = "https://as-cdn2.top/video/fedcba9876543210fedcba9876543210?token=second-secret"
	)
	var gotMu sync.Mutex
	var requests []requestRecord
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		original := r.Header.Get("X-Test-Original-URL")
		requestBody, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(requestBody))
		gotMu.Lock()
		requests = append(requests, requestRecord{method: r.Method, url: original, cookie: r.Header.Get("Cookie"), referer: r.Header.Get("Referer"), requested: r.Header.Get("X-Requested-With"), contentType: r.Header.Get("Content-Type"), body: string(requestBody)})
		gotMu.Unlock()
		switch {
		case r.URL.Path == "/api/v1/server/episode" && r.Method == http.MethodGet:
			io.WriteString(w, `{"success":true,"data":{"sub":[{"url":"`+first+`"},{"url":"`+first+`"},{"url":"https://abyss.example/video/x"},{"url":"https://abyss.example/video/x"},{"url":"https://as-cdn3.top.evil/video/0123456789abcdef0123456789abcdef"},{"url":"https://user@as-cdn4.top/video/0123456789abcdef0123456789abcdef"},{"url":"http://as-cdn5.top/video/0123456789abcdef0123456789abcdef"}],"dub":[{"url":"`+second+`"},{"url":"https://as-cdn6.top/video/never-attempted"},{"url":"https://abyss.example/never-attempted"}]}}`)
		case r.URL.Path == "/video/0123456789abcdef0123456789abcdef":
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "cookie-secret", Path: "/"})
			io.WriteString(w, `<html>playerjsSubtitle="[English]subs.jpg"</html>`)
		case r.URL.Path == "/video/fedcba9876543210fedcba9876543210":
			io.WriteString(w, `<html>playerjsSubtitle="[English]subs.jpg"</html>`)
		case r.URL.Path == "/player/index.php":
			if r.FormValue("hash") == "0123456789abcdef0123456789abcdef" {
				if r.Header.Get("Cookie") != "session=cookie-secret" {
					t.Errorf("first POST did not receive player cookie: %q", r.Header.Get("Cookie"))
				}
				http.Error(w, "upstream-secret Authorization: Bearer header-secret "+first, http.StatusBadGateway)
			} else {
				if r.Header.Get("Cookie") != "" {
					t.Errorf("fallback POST leaked cookie: %q", r.Header.Get("Cookie"))
				}
				io.WriteString(w, `{"hls":true,"videoSource":"/media/master.m3u8?sig=master%2Ctwo"}`)
			}
		case r.URL.Path == "/media/master.m3u8":
			io.WriteString(w, "#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"eng\",NAME=\"English\",LANGUAGE=\"eng\",URI=\"audio.m3u8?sig=a%2Cb\"\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"jpn\",NAME=\"Japanese\",LANGUAGE=\"jpn\",URI=\"audio-jpn.m3u8\"\n#EXT-X-STREAM-INF:BANDWIDTH=100,RESOLUTION=1280x720,AUDIO=\"eng\"\nchild/video.m3u8?token=x%2Cy\n#EXT-X-STREAM-INF:BANDWIDTH=90,RESOLUTION=1280x720,AUDIO=\"jpn\"\nchild/video-jpn.m3u8\n")
		case r.URL.Path == "/media/child/video.m3u8":
			io.WriteString(w, "#EXTM3U\n")
		case r.URL.Path == "/video/subs.jpg":
			if r.Header.Get("Cookie") != "" {
				t.Errorf("subtitle request leaked cookie: %q", r.Header.Get("Cookie"))
			}
			io.WriteString(w, "1\n00:00:01,23 --> 00:00:02,7\nHello\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	base, _ := url.Parse(server.URL)
	transport := &rerouteTransport{base: base}
	httpClient := &http.Client{Transport: transport, Timeout: time.Second}
	jar, _ := cookiejar.New(nil)
	for _, host := range []string{"https://as-cdn1.top/", "https://as-cdn2.top/"} {
		u, _ := url.Parse(host)
		jar.SetCookies(u, []*http.Cookie{{Name: "caller", Value: "must-not-leak"}})
	}
	httpClient.Jar = jar
	c := NewClientWithEndpoints(httpClient, "https://animesalt.backendapia.workers.dev/api/v1", "https://animesalt.cx")
	t.Cleanup(c.Close)
	var logs bytes.Buffer
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ctx = streamlog.WithProvider(streamlog.WithResolution(ctx, log.New(&logs, "", 0), 123, 1, "sub"), "animeflix")
	streams, err := c.Streams(ctx, provider.Episode{ShowID: "show", Number: 1, Value: "episode"}, "sub", "best")
	if err != nil || len(streams) != 1 {
		t.Fatalf("Streams() = %#v, %v", streams, err)
	}
	if streams[0].Subtitle == "" || streams[0].Headers["Referer"] != second {
		t.Fatalf("stream = %#v, want local subtitles and second player referer", streams[0])
	}
	cancel()
	if streams[0].Source != "mystream-6" {
		t.Fatalf("Source = %q", streams[0].Source)
	}
	for source, outcome := range map[string]string{
		"mystream-1": "failed reason=resolution_failed",
		"source-2":   "unavailable reason=unsupported_source",
		"source-3":   "unavailable reason=unsupported_source",
		"source-4":   "unavailable reason=unsupported_source",
		"source-5":   "unavailable reason=unsupported_source",
		"mystream-6": "resolved reason=-",
	} {
		prefix := "provider=animeflix source=" + source + " stage=resolve status="
		if strings.Count(logs.String(), prefix+"started ") != 1 || strings.Count(logs.String(), prefix+outcome+" ") != 1 {
			t.Fatalf("missing or duplicate outcome for %s: %s", source, logs.String())
		}
	}
	if strings.Count(logs.String(), "stream_attempt ") != 12 {
		t.Fatalf("unexpected attempts: %s", logs.String())
	}
	for _, secret := range []string{first, second, "https://", "http://", "first-secret", "second-secret", "cookie-secret", "upstream-secret", "header-secret", "sig=", "token=", userAgent} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("logs contain secret %q: %s", secret, logs.String())
		}
	}
	repeated, err := c.Streams(t.Context(), provider.Episode{ShowID: "show", Number: 1, Value: "episode"}, "sub", "best")
	if err != nil || len(repeated) != 1 || repeated[0].Source != streams[0].Source {
		t.Fatalf("unstable winning Source: %#v, %v", repeated, err)
	}
	for _, assetURL := range []string{streams[0].URL, streams[0].Subtitle} {
		response, getErr := http.Get(assetURL)
		if getErr != nil {
			t.Fatal(getErr)
		}
		body, readErr := io.ReadAll(response.Body)
		response.Body.Close()
		if readErr != nil || response.StatusCode != http.StatusOK || len(body) == 0 {
			t.Fatalf("asset %s: status=%d err=%v body=%q", assetURL, response.StatusCode, readErr, body)
		}
		if assetURL == streams[0].URL && (!strings.Contains(string(body), "#EXTM3U") || !strings.Contains(string(body), `GROUP-ID="jpn"`)) {
			t.Fatalf("master payload did not contain selected Japanese audio: %s", body)
		}
		if assetURL == streams[0].Subtitle && (!strings.HasPrefix(string(body), "WEBVTT") || !strings.Contains(string(body), "Hello")) {
			t.Fatalf("subtitle payload = %q", body)
		}
	}
	gotMu.Lock()
	requests = append([]requestRecord(nil), requests...)
	gotMu.Unlock()
	var sawAPI, sawPost, sawMaster, sawSubtitle bool
	for _, request := range requests {
		switch {
		case strings.HasPrefix(request.url, c.api):
			sawAPI = request.method == http.MethodGet && request.cookie == ""
		case strings.Contains(request.url, "/player/index.php"):
			parsed, parseErr := url.Parse(request.url)
			query := parsed.Query()
			sawPost = parseErr == nil && request.method == http.MethodPost && request.requested == "XMLHttpRequest" && request.contentType == "application/x-www-form-urlencoded; charset=UTF-8" && request.referer == second && request.cookie == "" && query.Get("data") == "fedcba9876543210fedcba9876543210" && query.Get("do") == "getVideo" && strings.Contains(request.body, "hash=fedcba9876543210fedcba9876543210") && strings.Contains(request.body, "r=https%3A%2F%2Fanimesalt.cx%2F")
		case strings.Contains(request.url, "/media/master.m3u8"):
			sawMaster = request.cookie == "" && strings.Contains(request.url, "sig=master%2Ctwo")
		case strings.Contains(request.url, "/video/subs.jpg"):
			sawSubtitle = request.cookie == ""
		}
	}
	if !sawAPI || !sawPost || !sawMaster || !sawSubtitle {
		t.Fatalf("request properties API=%v POST=%v master=%v subtitle=%v; requests=%#v", sawAPI, sawPost, sawMaster, sawSubtitle, requests)
	}
	for _, rejected := range []string{"abyss.example", "as-cdn3.top.evil", "as-cdn4.top", "as-cdn5.top", "never-attempted"} {
		for _, request := range requests {
			if strings.Contains(request.url, rejected) {
				t.Fatalf("unsupported source %q was requested: %s", rejected, request.url)
			}
		}
	}
	for _, host := range []string{"https://as-cdn1.top/", "https://as-cdn2.top/"} {
		u, _ := url.Parse(host)
		if cookies := httpClient.Jar.Cookies(u); len(cookies) != 1 || cookies[0].Name != "caller" {
			t.Fatalf("caller jar changed for %s: %#v", host, cookies)
		}
	}
}

func TestStreamsConcurrentResolutionsDoNotShareCookieSessions(t *testing.T) {
	ids := map[string]string{"one": "11111111111111111111111111111111", "two": "22222222222222222222222222222222"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v1/server/") {
			id := strings.TrimPrefix(r.URL.Path, "/api/v1/server/")
			io.WriteString(w, `{"success":true,"data":{"dub":[{"url":"https://as-cdn1.top/video/`+ids[id]+`"}]}}`)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/video/") {
			id := strings.TrimPrefix(r.URL.Path, "/video/")
			http.SetCookie(w, &http.Cookie{Name: "session", Value: id, Path: "/"})
			io.WriteString(w, "player")
			return
		}
		if r.URL.Path == "/player/index.php" {
			if r.Header.Get("Cookie") != "session="+r.FormValue("hash") {
				http.Error(w, "wrong session", http.StatusUnauthorized)
				return
			}
			io.WriteString(w, `{"hls":true,"videoSource":"/media/`+r.FormValue("hash")+`.m3u8"}`)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/media/") {
			if r.Header.Get("Cookie") != "" {
				http.Error(w, "media received cookie", http.StatusUnauthorized)
				return
			}
			io.WriteString(w, "#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"e\",NAME=\"English\",LANGUAGE=\"eng\",URI=\"a.m3u8\"\n#EXT-X-STREAM-INF:RESOLUTION=720x480,AUDIO=\"e\"\nv.m3u8\n")
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	base, _ := url.Parse(server.URL)
	transport := &rerouteTransport{base: base}
	httpClient := &http.Client{Transport: transport}
	c := NewClientWithEndpoints(httpClient, server.URL+"/api/v1", "https://animesalt.cx")
	t.Cleanup(c.Close)
	var wg sync.WaitGroup
	errs := make(chan error, len(ids))
	for episode := range ids {
		wg.Add(1)
		go func(episode string) {
			defer wg.Done()
			streams, err := c.Streams(context.Background(), provider.Episode{ShowID: "s", Number: 1, Value: episode}, "dub", "")
			if err == nil && (len(streams) != 1 || streams[0].Subtitle != "") {
				err = errors.New("dub stream unexpectedly includes subtitles")
			}
			errs <- err
		}(episode)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if httpClient.Jar != nil {
		t.Fatal("Streams mutated the caller's cookie jar")
	}
}

func TestStreamsRejectsAPIFailureOversizeAndCancellation(t *testing.T) {
	for name, response := range map[string]string{
		"failure":  `{"success":false,"data":null}`,
		"oversize": `{"success":true,"data":"` + strings.Repeat("x", maxBody+1) + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, response) }))
			defer server.Close()
			_, err := NewClientWithEndpoints(server.Client(), server.URL, "https://origin.example").Streams(context.Background(), provider.Episode{ShowID: "s", Number: 1, Value: "e"}, "sub", "")
			if err == nil || !strings.Contains(err.Error(), "AnimeFlix") {
				t.Fatalf("error = %v", err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := NewClientWithEndpoints(nil, "https://example.invalid", "https://origin.example").Streams(ctx, provider.Episode{ShowID: "s", Number: 1, Value: "e"}, "sub", "")
	if !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("canceled Streams error = %v", err)
	}
}

func TestStreamsRejectsUnverifiedPlayerResults(t *testing.T) {
	for name, result := range map[string]string{
		"missing English subtitle": "playerjsSubtitle=\"[Spanish]sub.jpg\"",
		"invalid English subtitle": "playerjsSubtitle=\"[English]javascript:bad\"",
		"wrong audio":              "audio",
		"missing HLS player":       "missing-hls",
		"invalid player JSON":      "not-json",
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasPrefix(r.URL.Path, "/api/v1/server/"):
					io.WriteString(w, `{"success":true,"data":{"sub":[{"url":"https://as-cdn1.top/video/0123456789abcdef0123456789abcdef"}]}}`)
				case strings.HasPrefix(r.URL.Path, "/video/"):
					io.WriteString(w, "<html>"+result+"</html>")
				case r.URL.Path == "/player/index.php":
					switch result {
					case "audio":
						io.WriteString(w, `{"hls":true,"videoSource":"/master.m3u8"}`)
					case "missing-hls":
						io.WriteString(w, `{"videoSource":"/master.m3u8"}`)
					case "not-json":
						io.WriteString(w, "not json")
					default:
						io.WriteString(w, `{"hls":true,"videoSource":"/master.m3u8"}`)
					}
				case r.URL.Path == "/master.m3u8":
					if result == "audio" {
						io.WriteString(w, "#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"e\",NAME=\"English\",LANGUAGE=\"eng\",URI=\"a\"\n#EXT-X-STREAM-INF:AUDIO=\"e\"\nv\n")
					} else {
						io.WriteString(w, "#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"j\",NAME=\"Japanese\",LANGUAGE=\"jpn\",URI=\"a\"\n#EXT-X-STREAM-INF:AUDIO=\"j\"\nv\n")
					}
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			base, _ := url.Parse(server.URL)
			c := NewClientWithEndpoints(&http.Client{Transport: &rerouteTransport{base: base}}, server.URL+"/api/v1", "https://animeflix.com.ru")
			t.Cleanup(c.Close)
			var logs bytes.Buffer
			ctx := streamlog.WithProvider(streamlog.WithResolution(t.Context(), log.New(&logs, "", 0), 123, 1, "sub"), "animeflix")
			if _, err := c.Streams(ctx, provider.Episode{ShowID: "s", Number: 1, Value: "e"}, "sub", ""); err == nil {
				t.Fatal("Streams accepted an unverified result")
			}
			outcome := "failed reason=resolution_failed"
			if name == "missing English subtitle" {
				outcome = "unavailable reason=missing_subtitles"
			}
			if strings.Count(logs.String(), "stream_attempt ") != 2 || !strings.Contains(logs.String(), "status="+outcome+" ") {
				t.Fatalf("unexpected rejection outcome: %s", logs.String())
			}
		})
	}
}
