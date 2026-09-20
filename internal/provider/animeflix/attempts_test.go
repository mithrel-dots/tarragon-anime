package animeflix

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"testing"
	"time"

	"tarragon-anime/internal/provider"
	"tarragon-anime/internal/streamlog"
)

type attemptTransport func(*http.Request) (*http.Response, error)

func (f attemptTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

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
			ctx = streamlog.WithProvider(streamlog.WithResolution(ctx, log.New(&logs, "", 0), 123, 1, "dub"), "animeflix")
			calls := 0
			client := &http.Client{Transport: attemptTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				var body string
				switch r.URL.Path {
				case "/server/episode":
					body = `{"success":true,"data":{"dub":[{"url":"https://as-cdn1.top/video/0123456789abcdef0123456789abcdef?token=query-secret"},{"url":"https://as-cdn2.top/video/never-attempted"}]}}`
				case "/video/0123456789abcdef0123456789abcdef":
					if phase == "source" {
						cancel(streamlog.ErrWinnerSelected)
						return nil, errors.New("https://secret.test/?token=query-secret Cookie: cookie-secret")
					}
					if phase == "timeout" {
						<-ctx.Done()
						return nil, ctx.Err()
					}
					body = "player"
				case "/player/index.php":
					body = `{"hls":true,"videoSource":"/master.m3u8"}`
				case "/master.m3u8":
					body = "#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"e\",LANGUAGE=\"eng\",URI=\"audio.m3u8\"\n#EXT-X-STREAM-INF:AUDIO=\"e\"\nvideo.m3u8\n"
					cancel(nil) // A late successful response must not publish assets.
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})}
			c := NewClientWithEndpoints(client, "https://api.test", "https://origin.test")
			defer c.Close()
			if phase == "before" {
				cancel(nil)
			}
			streams, err := c.Streams(ctx, provider.Episode{ShowID: "show", Number: 1, Value: "episode"}, "dub", "best")
			if len(streams) != 0 || !errors.Is(err, ctx.Err()) || ctx.Err() == nil {
				t.Fatalf("Streams = %#v, %v; ctx = %v", streams, err, ctx.Err())
			}
			if c.assets != nil || c.closed {
				t.Fatal("cancelled lookup created assets or closed the shared client")
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
			prefix := "source=mystream-1 stage=resolve status="
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

func TestPublishRejectsCancelledContextWithoutAffectingWinner(t *testing.T) {
	c := NewClient(nil)
	defer c.Close()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := c.publish(ctx, []byte("cancelled"), nil); !errors.Is(err, context.Canceled) || c.assets != nil {
		t.Fatalf("cancelled publish created an asset server: %v", err)
	}
	if _, _, err := c.publish(t.Context(), []byte("winner"), nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.publish(ctx, []byte("cancelled"), nil); !errors.Is(err, context.Canceled) || len(c.assets.entries) != 1 || c.closed {
		t.Fatalf("cancelled publish changed winner assets: %v", err)
	}
	ctx, cancel = context.WithCancel(t.Context())
	defer cancel()
	c.assetsMu.Lock()
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		_, _, err := c.publish(ctx, []byte("cancelled while waiting"), nil)
		done <- err
	}()
	<-started
	cancel()
	c.assetsMu.Unlock()
	if err := <-done; !errors.Is(err, context.Canceled) || len(c.assets.entries) != 1 {
		t.Fatalf("publish did not recheck cancellation after locking: %v", err)
	}
}
