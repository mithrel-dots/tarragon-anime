package anineko

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
	"testing"
	"time"

	"tarragon-anime/internal/streamlog"
)

type attemptTransport func(*http.Request) (*http.Response, error)

func (f attemptTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestStreamsAttemptLogsAndWinnerLifetime(t *testing.T) {
	for _, host := range []string{"bibiemb", "vibeplayer"} {
		t.Run(host, func(t *testing.T) {
			winner := "https://" + host + ".test/embed/winner?token=query-secret"
			payload := []byte{0x47, 0x11, 0x22, 0x33}
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/watch/show/ep-1":
					io.WriteString(w, `<div data-id="sub"><div data-video="https://unknown.test/secret?token=unsupported-secret"></div><div data-video="https://unknown.test/secret?token=unsupported-secret"></div><div data-video="https://bibiemb.test/embed/failure?token=failure-secret"></div><div data-video="https://bibiemb.test/embed/failure?token=failure-secret"></div><div data-video="`+winner+`"></div><div data-video="https://bibiemb.test/never-attempted"></div><div data-video="https://unknown.test/never-attempted"></div></div>`)
				case "/embed/failure":
					if r.Header.Get("Cookie") != "session=cookie-secret" {
						t.Errorf("missing fixture cookie: %q", r.Header.Get("Cookie"))
					}
					http.Error(w, "https://secret.test/?token=body-secret Authorization: Bearer header-secret Cookie: cookie-secret", http.StatusBadGateway)
				case "/embed/winner":
					io.WriteString(w, `const src = "`+server.URL+`/public/stream/path-secret/master.m3u8?sig=media-secret"; const subtitle = "https://subs.test/subtitle-secret.vtt";`)
				case "/public/stream/path-secret/master.m3u8":
					io.WriteString(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1,RESOLUTION=1280x720\n720.m3u8\n#EXT-X-STREAM-INF:BANDWIDTH=2,RESOLUTION=1920x1080\n1080.m3u8\n")
				case "/public/stream/path-secret/1080.m3u8":
					io.WriteString(w, "#EXTM3U\n#EXTINF:4,\nsegment.ts\n")
				case "/public/stream/path-secret/segment.ts":
					if host == "vibeplayer" {
						w.Write(append([]byte("PNG wrapper"), pngIENDMarker...))
					}
					w.Write(payload)
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			target, _ := url.Parse(server.URL)
			jar, _ := cookiejar.New(nil)
			cookieURL, _ := url.Parse("https://bibiemb.test")
			jar.SetCookies(cookieURL, []*http.Cookie{{Name: "session", Value: "cookie-secret"}})
			httpClient := &http.Client{Jar: jar, Transport: attemptTransport(func(r *http.Request) (*http.Response, error) {
				clone := r.Clone(r.Context())
				clone.URL.Scheme, clone.URL.Host = target.Scheme, target.Host
				return http.DefaultTransport.RoundTrip(clone)
			})}
			var logs bytes.Buffer
			logger := log.New(&logs, "", 0)
			client := NewClientWithBaseURL(httpClient, server.URL).WithLogger(logger)
			defer client.Close()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			ctx = streamlog.WithProvider(streamlog.WithResolution(ctx, logger, 123, 1, "sub"), "anineko")
			streams, err := client.Streams(ctx, Episode{ShowID: "show", Number: 1}, "sub", "best")
			if err != nil || len(streams) != 1 {
				t.Fatalf("Streams = %#v, %v", streams, err)
			}
			cancel()
			stream := streams[0]
			if stream.Source != "source-3" || stream.Headers["Referer"] != winner || stream.Subtitle != "https://subs.test/subtitle-secret.vtt" {
				t.Fatalf("unexpected winning metadata: %#v", stream)
			}
			for source, outcome := range map[string]string{
				"source-1": "unavailable reason=unsupported_source",
				"source-2": "failed reason=resolution_failed",
				"source-3": "resolved reason=-",
			} {
				prefix := "provider=anineko source=" + source + " stage=resolve status="
				if strings.Count(logs.String(), prefix+"started ") != 1 || strings.Count(logs.String(), prefix+outcome+" ") != 1 {
					t.Fatalf("missing or duplicate outcome for %s: %s", source, logs.String())
				}
			}
			if strings.Count(logs.String(), "stream_attempt ") != 6 {
				t.Fatalf("unexpected attempt logs: %s", logs.String())
			}
			for _, secret := range []string{"http://", "https://", "query-secret", "unsupported-secret", "failure-secret", "body-secret", "header-secret", "cookie-secret", "path-secret", "media-secret", "subtitle-secret", "token=", "Authorization", "Cookie", userAgent} {
				if strings.Contains(logs.String(), secret) {
					t.Fatalf("logs leaked %q: %s", secret, logs.String())
				}
			}
			get := func(endpoint string) []byte {
				t.Helper()
				req, _ := http.NewRequest(http.MethodGet, endpoint, nil)
				for name, value := range stream.Headers {
					req.Header.Set(name, value)
				}
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				body, err := io.ReadAll(resp.Body)
				if err != nil || resp.StatusCode != http.StatusOK {
					t.Fatalf("winner playback after lookup cancellation: status=%d err=%v", resp.StatusCode, err)
				}
				return body
			}
			variantURL := stream.URL
			if host == "vibeplayer" {
				master := get(stream.URL)
				variantURL = chooseVariant(parseVariants(stream.URL, string(master)), "best").URL
			} else if !strings.HasSuffix(variantURL, "/1080.m3u8") {
				t.Fatalf("quality changed: %s", variantURL)
			}
			variant := strings.Fields(string(get(variantURL)))
			segmentURL := resolvePlaylistURL(variantURL, variant[len(variant)-1])
			if body := get(segmentURL); !bytes.Equal(body, payload) {
				t.Fatalf("segment payload = %v, want %v", body, payload)
			}
			repeated, err := client.Streams(t.Context(), Episode{ShowID: "show", Number: 1}, "sub", "best")
			if err != nil || len(repeated) != 1 || repeated[0].Source != stream.Source {
				t.Fatalf("unstable winning Source: %#v, %v", repeated, err)
			}
		})
	}
}

func TestAttemptCancellationAndTimeout(t *testing.T) {
	for _, phase := range []string{"before", "source", "publish", "timeout"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			if phase == "timeout" {
				var stop context.CancelFunc
				ctx, stop = context.WithTimeout(ctx, 250*time.Millisecond)
				defer stop()
			}
			var logs bytes.Buffer
			logger := log.New(&logs, "", 0)
			ctx = streamlog.WithProvider(streamlog.WithResolution(ctx, logger, 123, 1, "sub"), "anineko")
			calls := 0
			httpClient := &http.Client{Transport: attemptTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				body := `<div data-id="sub"><div data-video="https://vibeplayer.test/embed?token=query-secret"></div><div data-video="https://bibiemb.test/never-attempted"></div></div>`
				if r.URL.Path == "/embed" {
					if phase == "timeout" {
						<-ctx.Done()
						return nil, ctx.Err()
					}
					if phase == "source" {
						cancel(streamlog.ErrWinnerSelected)
						return nil, errors.New("https://secret.test/?token=query-secret Cookie: cookie-secret")
					}
					cancel(nil)
					body = `const src = "https://media.test/public/stream/secret/master.m3u8"`
				} else if r.URL.Path != "/watch/show/ep-1" {
					t.Errorf("unexpected request: %s", r.URL.Path)
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})}
			client := NewClientWithBaseURL(httpClient, "https://anineko.test").WithLogger(logger)
			defer client.Close()
			if phase == "before" {
				cancel(nil)
			}
			streams, err := client.Streams(ctx, Episode{ShowID: "show", Number: 1}, "sub", "best")
			if len(streams) != 0 || !errors.Is(err, ctx.Err()) || ctx.Err() == nil {
				t.Fatalf("Streams = %#v, %v; ctx = %v", streams, err, ctx.Err())
			}
			if client.proxy != nil {
				t.Fatal("cancelled lookup created a proxy")
			}
			if phase == "before" {
				if calls != 0 || logs.Len() != 0 {
					t.Fatalf("pre-cancelled lookup did work: calls=%d logs=%s", calls, logs.String())
				}
				return
			}
			outcome := "cancelled reason=request_cancelled"
			if phase == "source" {
				outcome = "cancelled reason=winner_selected"
			} else if phase == "timeout" {
				outcome = "timeout reason=deadline_exceeded"
			}
			prefix := "source=source-1 stage=resolve status="
			if strings.Count(logs.String(), "stream_attempt ") != 2 || strings.Count(logs.String(), prefix+"started ") != 1 || strings.Count(logs.String(), prefix+outcome+" ") != 1 {
				t.Fatalf("expected exactly one terminal outcome: %s", logs.String())
			}
			for _, secret := range []string{"https://", "query-secret", "Cookie", "cookie-secret"} {
				if strings.Contains(logs.String(), secret) {
					t.Fatalf("logs leaked %q: %s", secret, logs.String())
				}
			}
		})
	}
}

func TestProxyRejectsCancelledRegistrationWithoutAffectingWinner(t *testing.T) {
	client := NewClient(nil)
	defer client.Close()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := client.vibeProxy(ctx); !errors.Is(err, context.Canceled) || client.proxy != nil {
		t.Fatalf("cancelled lookup created a proxy: %v", err)
	}
	proxy, err := client.vibeProxy(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := proxy.register(t.Context(), "https://media.test/master.m3u8", "https://vibeplayer.test/embed"); err != nil {
		t.Fatal(err)
	}
	if _, err := proxy.register(ctx, "https://media.test/cancelled.m3u8", "https://vibeplayer.test/embed"); !errors.Is(err, context.Canceled) || len(proxy.sessions) != 1 || client.proxy != proxy {
		t.Fatalf("cancelled lookup changed winner sessions: %v", err)
	}
	ctx, cancel = context.WithCancel(t.Context())
	defer cancel()
	proxy.mu.Lock()
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		_, err := proxy.register(ctx, "https://media.test/cancelled.m3u8", "https://vibeplayer.test/embed")
		done <- err
	}()
	<-started
	cancel()
	proxy.mu.Unlock()
	if err := <-done; !errors.Is(err, context.Canceled) || len(proxy.sessions) != 1 {
		t.Fatalf("register did not recheck cancellation after locking: %v", err)
	}
}
