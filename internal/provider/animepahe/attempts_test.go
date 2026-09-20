package animepahe

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"tarragon-anime/internal/provider"
	"tarragon-anime/internal/streamlog"
)

type attemptTransport func(*http.Request) (*http.Response, error)

func (f attemptTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestStreamsAttemptLogsPreserveQualityOrder(t *testing.T) {
	for _, quality := range []string{"best", "worst"} {
		t.Run(quality, func(t *testing.T) {
			unsupportedRes, firstRes, lastRes := 2160, 1080, 480
			if quality == "worst" {
				unsupportedRes, firstRes, lastRes = 240, 480, 1080
			}
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/play/session/episode":
					fmt.Fprintf(w, `<button data-src="https://unknown.test/secret?token=unsupported-secret" data-resolution="%d" data-audio="jpn"><button data-src="https://unknown.test/secret?token=unsupported-secret" data-resolution="%d" data-audio="jpn"><button data-src="https://kwik.cx/e/failure?token=failure-secret" data-resolution="%d" data-audio="jpn"><button data-src="https://kwik.cx/e/failure?token=failure-secret" data-resolution="%d" data-audio="jpn"><button data-src="https://kwik.si/e/winner?token=query-secret" data-resolution="720" data-audio="jpn"><button data-src="https://kwik.cx/e/never-attempted" data-resolution="%d" data-audio="jpn"><button data-src="https://unknown.test/never-attempted" data-resolution="%d" data-audio="jpn"><button data-src="https://kwik.cx/e/wrong-language" data-resolution="720" data-audio="eng">`, unsupportedRes, unsupportedRes, firstRes, firstRes, lastRes, lastRes)
				case "/e/failure":
					if r.Header.Get("Cookie") != "session=cookie-secret" {
						t.Errorf("missing fixture cookie: %q", r.Header.Get("Cookie"))
					}
					http.Error(w, "https://secret.test/?token=body-secret Authorization: Bearer header-secret Cookie: cookie-secret", http.StatusBadGateway)
				case "/e/winner":
					fmt.Fprintf(w, `eval(function(p,a,c,k,e,d){return p}('0',62,1,'%s/media/stream.m3u8?sig=media-secret'.split('|'),0,{}))`, server.URL)
				case "/media/stream.m3u8":
					if r.Header.Get("Referer") != "https://kwik.si/" {
						t.Errorf("playback referer = %q", r.Header.Get("Referer"))
					}
					io.WriteString(w, "#EXTM3U\n")
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			target, _ := url.Parse(server.URL)
			client := NewClientWithBaseURL(&http.Client{Transport: attemptTransport(func(r *http.Request) (*http.Response, error) {
				clone := r.Clone(r.Context())
				clone.URL.Scheme, clone.URL.Host = target.Scheme, target.Host
				return http.DefaultTransport.RoundTrip(clone)
			})}, server.URL)
			client.solver = func(context.Context, string) ([]*http.Cookie, error) {
				t.Error("unexpected challenge solver")
				return nil, errors.New("browser forbidden in test")
			}
			cookieURL, _ := url.Parse("https://kwik.cx")
			client.http.Jar.SetCookies(cookieURL, []*http.Cookie{{Name: "session", Value: "cookie-secret"}})
			var logs bytes.Buffer
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			ctx = streamlog.WithProvider(streamlog.WithResolution(ctx, log.New(&logs, "", 0), 123, 1, "sub"), "animepahe")
			episode := provider.Episode{ShowID: "1:session", Number: 1, Value: "episode"}
			streams, err := client.Streams(ctx, episode, "sub", quality)
			if err != nil || len(streams) != 1 {
				t.Fatalf("Streams = %#v, %v", streams, err)
			}
			cancel()
			stream := streams[0]
			if stream.Source != "kwik-3" || stream.URL != server.URL+"/media/stream.m3u8?sig=media-secret" || stream.Headers["Referer"] != "https://kwik.si/" {
				t.Fatalf("unexpected winning metadata: %#v", stream)
			}
			for source, outcome := range map[string]string{
				"source-1": "unavailable reason=unsupported_source",
				"kwik-2":   "failed reason=resolution_failed",
				"kwik-3":   "resolved reason=-",
			} {
				prefix := "provider=animepahe source=" + source + " stage=resolve status="
				if strings.Count(logs.String(), prefix+"started ") != 1 || strings.Count(logs.String(), prefix+outcome+" ") != 1 {
					t.Fatalf("missing or duplicate outcome for %s: %s", source, logs.String())
				}
			}
			if strings.Count(logs.String(), "stream_attempt ") != 6 {
				t.Fatalf("unexpected attempt logs: %s", logs.String())
			}
			for _, secret := range []string{"http://", "https://", "query-secret", "unsupported-secret", "failure-secret", "body-secret", "header-secret", "cookie-secret", "media-secret", "token=", "Authorization", "Cookie", userAgent} {
				if strings.Contains(logs.String(), secret) {
					t.Fatalf("logs leaked %q: %s", secret, logs.String())
				}
			}
			req, _ := http.NewRequest(http.MethodGet, stream.URL, nil)
			for name, value := range stream.Headers {
				req.Header.Set(name, value)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil || resp.StatusCode != http.StatusOK || !bytes.HasPrefix(body, []byte("#EXTM3U")) {
				t.Fatalf("winner playback after cancellation: status=%d err=%v", resp.StatusCode, err)
			}
			repeated, err := client.Streams(t.Context(), episode, "sub", quality)
			if err != nil || len(repeated) != 1 || repeated[0].Source != stream.Source {
				t.Fatalf("unstable winning Source: %#v, %v", repeated, err)
			}
		})
	}
}

func TestAttemptCancellationAndTimeout(t *testing.T) {
	for _, phase := range []string{"before", "source", "resolved", "challenge", "timeout"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			if phase == "timeout" {
				var stop context.CancelFunc
				ctx, stop = context.WithTimeout(ctx, 250*time.Millisecond)
				defer stop()
			}
			var logs bytes.Buffer
			ctx = streamlog.WithProvider(streamlog.WithResolution(ctx, log.New(&logs, "", 0), 123, 1, "sub"), "animepahe")
			calls := 0
			httpClient := &http.Client{Transport: attemptTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				body := `<button data-src="https://kwik.cx/e/first?token=query-secret" data-resolution="1080" data-audio="jpn"><button data-src="https://kwik.si/e/never-attempted" data-resolution="720" data-audio="jpn">`
				status := http.StatusOK
				if r.URL.Path == "/e/first" {
					if phase == "timeout" {
						<-ctx.Done()
						return nil, ctx.Err()
					}
					if phase == "source" {
						cancel(streamlog.ErrWinnerSelected)
						return nil, errors.New("https://secret.test/?token=query-secret Cookie: cookie-secret")
					}
					cancel(nil)
					body = `eval(function(p,a,c,k,e,d){return p}('0',62,1,'https://media.test/stream.m3u8?token=media-secret'.split('|'),0,{}))`
					if phase == "challenge" {
						status, body = http.StatusForbidden, "Just a moment..."
					}
				} else if r.URL.Path != "/play/session/episode" {
					t.Errorf("unexpected request: %s", r.URL.Path)
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})}
			client := NewClientWithBaseURL(httpClient, "https://animepahe.test")
			client.solver = func(context.Context, string) ([]*http.Cookie, error) {
				t.Error("cancelled lookup invoked the challenge solver")
				return nil, errors.New("browser forbidden in test")
			}
			if phase == "before" {
				cancel(nil)
			}
			streams, err := client.Streams(ctx, provider.Episode{ShowID: "1:session", Number: 1, Value: "episode"}, "sub", "best")
			if len(streams) != 0 || !errors.Is(err, ctx.Err()) || ctx.Err() == nil {
				t.Fatalf("Streams = %#v, %v; ctx = %v", streams, err, ctx.Err())
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
			prefix := "source=kwik-1 stage=resolve status="
			if strings.Count(logs.String(), "stream_attempt ") != 2 || strings.Count(logs.String(), prefix+"started ") != 1 || strings.Count(logs.String(), prefix+outcome+" ") != 1 {
				t.Fatalf("expected exactly one terminal outcome: %s", logs.String())
			}
			for _, secret := range []string{"https://", "query-secret", "Cookie", "cookie-secret", "media-secret"} {
				if strings.Contains(logs.String(), secret) {
					t.Fatalf("logs leaked %q: %s", secret, logs.String())
				}
			}
		})
	}
}

func TestCancelledBypassDoesNotSolveOrCacheCookies(t *testing.T) {
	for _, phase := range []string{"before", "solver"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			client := NewClient(nil)
			calls := 0
			client.solver = func(context.Context, string) ([]*http.Cookie, error) {
				calls++
				cancel()
				return []*http.Cookie{{Name: "clearance", Value: "cookie-secret"}}, nil
			}
			if phase == "before" {
				cancel()
			}
			endpoint, _ := url.Parse("https://kwik.cx/e/secret")
			if err := client.bypass(ctx, endpoint.String()); !errors.Is(err, context.Canceled) {
				t.Fatalf("bypass error = %v", err)
			}
			if len(client.http.Jar.Cookies(endpoint)) != 0 || (phase == "before" && calls != 0) {
				t.Fatal("cancelled bypass solved a challenge or cached clearance cookies")
			}
		})
	}
}
