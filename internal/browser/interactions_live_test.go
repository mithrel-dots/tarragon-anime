//go:build live

package browser

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-rod/rod/lib/proto"
)

// Exercise the same shared process even when callers copy the engine for logging.
type copiedCaptureEngine struct{ *chromeEngine }

func (e copiedCaptureEngine) capture(ctx context.Context, req Request) ([]Candidate, error) {
	copy := *e.chromeEngine
	return copy.capture(ctx, req)
}

func TestLiveLocalConcurrentInteractions(t *testing.T) {
	bin, err := FindBinary()
	if err != nil {
		t.Skip(err)
	}
	for _, mode := range []string{"source", "play", "source-play"} {
		t.Run(mode, func(t *testing.T) {
			var mu sync.Mutex
			clicks := map[string][]string{}
			loaded, media := make(chan string, 2), make(chan string, 2)
			release := make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				id := req.URL.Query().Get("id")
				switch req.URL.Path {
				case "/watch":
					w.Header().Set("Content-Type", "text/html")
					fmt.Fprintf(w, `<!doctype html><style>button{width:120px;height:40px}iframe{width:600px;height:350px}</style>
<script>const captureID=%q; const mark = kind => fetch('/click?id='+captureID+'&kind='+kind);</script>`, id)
					if mode == "source" {
						io.WriteString(w, `<button class="source" onclick="if(event.isTrusted){mark('source');fetch('/media.mp4?id='+captureID)}">Source</button>`)
					} else {
						target := "/middle?id=" + id
						if mode == "source-play" {
							target = "/empty"
						}
						fmt.Fprintf(w, `<iframe title="Episode player" src="%s"></iframe>`, target)
						if mode == "source-play" {
							io.WriteString(w, `<button class="source" onclick="if(event.isTrusted){mark('source');document.querySelector('iframe').src='/middle?id='+captureID}">Source</button>`)
						}
					}
					io.WriteString(w, `<script>addEventListener('load',()=>fetch('/loaded?id='+captureID))</script>`)
				case "/middle":
					w.Header().Set("Content-Type", "text/html")
					fmt.Fprintf(w, `<!doctype html><iframe class="embed" style="width:500px;height:250px" src="/player?id=%s"></iframe>`, id)
				case "/player":
					w.Header().Set("Content-Type", "text/html")
					fmt.Fprintf(w, `<!doctype html><video></video><button class="play" style="width:100px;height:40px" onclick="if(event.isTrusted){fetch('/click?id=%s&kind=play');fetch('/media.mp4?id=%s')}">Play</button>`, id, id)
				case "/empty":
					w.Header().Set("Content-Type", "text/html")
					io.WriteString(w, "<!doctype html><title>empty</title>")
				case "/loaded":
					loaded <- id
				case "/click":
					mu.Lock()
					clicks[id] = append(clicks[id], req.URL.Query().Get("kind"))
					mu.Unlock()
				case "/media.mp4":
					media <- id
					select {
					case <-release:
					case <-req.Context().Done():
						return
					}
					w.Header().Set("Content-Type", "video/mp4")
					w.WriteHeader(http.StatusPartialContent)
					io.WriteString(w, "local media fixture")
				default:
					http.NotFound(w, req)
				}
			}))
			t.Cleanup(server.Close)
			t.Cleanup(unblock)
			r := New(Options{Binary: bin, ProfileDir: t.TempDir(), Headless: true})
			r.newEngine = func(ctx context.Context, opts Options) (engine, error) {
				eng, err := newChromeEngine(ctx, opts)
				if err != nil {
					return nil, err
				}
				return copiedCaptureEngine{eng.(*chromeEngine)}, nil
			}
			t.Cleanup(r.Close)
			ctx, cancel := context.WithTimeout(t.Context(), defaultTimeout)
			defer cancel()
			started := time.Now()
			type result struct {
				id    string
				found []Candidate
				err   error
			}
			results := make(chan result, 2)
			logs := map[string]*outcomeLog{"a": {}, "b": {}}
			for _, id := range []string{"a", "b"} {
				go func() {
					req := Request{PageURL: server.URL + "/watch?id=" + id, Settle: 100 * time.Millisecond, FallbackSelector: "button.source", Accept: func(c Candidate) bool { return c.URL == server.URL+"/media.mp4?id="+id }}
					if mode != "source" {
						req.PlayButtonPath = []string{`iframe[title="Episode player"]`, "iframe.embed", "button.play"}
					}
					found, err := r.Capture(traceCapture(ctx, logs[id]), req)
					results <- result{id, found, err}
				}()
			}
			for range 2 {
				select {
				case <-loaded:
				case <-time.After(5 * time.Second):
					t.Fatal("two navigations did not load concurrently")
				}
			}
			r.mu.Lock()
			active := r.active
			r.mu.Unlock()
			if active != 2 || cap(r.sem) != 2 {
				t.Fatal("test did not use the production two capture slots")
			}
			// Neither capture may finish until both have clicked and requested media.
			// A capture-wide lock (or a click lock held across settle) deadlocks here.
			received := map[string]bool{}
			for len(received) < 2 {
				select {
				case id := <-media:
					if (id != "a" && id != "b") || received[id] {
						t.Fatalf("unexpected media request identity %q", id)
					}
					received[id] = true
					t.Logf("capture %s requested media after %s", id, time.Since(started))
				case <-ctx.Done():
					mu.Lock()
					t.Logf("trusted clicks: %v", clicks)
					mu.Unlock()
					t.Fatalf("both captures did not request media: a=%s b=%s", logs["a"].String(), logs["b"].String())
				}
			}
			t.Logf("both captures requested media after %s", time.Since(started))
			unblock()
			for range 2 {
				out := <-results
				if out.err != nil || len(out.found) != 1 {
					t.Fatalf("capture %s: %v; logs=%s", out.id, out.err, logs[out.id].String())
				}
				wantSource := "default"
				want := map[string]string{"default": "resolved"}
				if mode != "play" {
					wantSource = "source-1"
					want["default"] = "unavailable"
					want[wantSource] = "resolved"
				}
				if out.found[0].Source != wantSource {
					t.Fatalf("capture %s source=%s", out.id, out.found[0].Source)
				}
				assertSourceOutcomes(t, logs[out.id].String(), want)
				if mode != "source" && !strings.Contains(logs[out.id].String(), "source="+wantSource+" stage=browser_play status=resolved reason=click_dispatched") {
					t.Fatal("missing play outcome")
				}
				mu.Lock()
				got := strings.Join(clicks[out.id], "-")
				mu.Unlock()
				if got != mode {
					t.Fatalf("capture %s trusted clicks=%s, want %s", out.id, got, mode)
				}
			}
		})
	}
}

func TestLiveLocalBackgroundClickProbe(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, `<!doctype html><button onclick="if(event.isTrusted)document.body.dataset.clicks=String(+(document.body.dataset.clicks||0)+1)">Click</button>`)
	}))
	t.Cleanup(server.Close)
	eng := localFallbackEngine(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	first, err := eng.browser.Context(ctx).Page(proto.TargetCreateTarget{URL: server.URL + "/first"})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.WaitLoad(); err != nil {
		t.Fatal(err)
	}
	second, err := eng.browser.Context(ctx).Page(proto.TargetCreateTarget{URL: server.URL + "/second"})
	if err != nil {
		t.Fatal(err)
	}
	if err := second.WaitLoad(); err != nil {
		t.Fatal(err)
	}
	button, err := first.Element("button")
	if err != nil {
		t.Fatal(err)
	}
	state, err := first.Eval(`() => document.visibilityState`)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- button.Click(proto.InputMouseButtonLeft, 1) }()
	completed := false
	select {
	case err = <-done:
		completed = true
	case <-time.After(500 * time.Millisecond):
	}
	t.Logf("background root visibility=%s click_completed=%t click_error=%v", state.Value.Str(), completed, err)
	if err := (proto.PageBringToFront{}).Call(first); err != nil {
		t.Fatal(err)
	}
	if !completed {
		select {
		case err = <-done:
		case <-time.After(time.Second):
			t.Fatal("bringing the root forward did not unblock the click")
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	after, err := first.Eval(`() => +(document.body.dataset.clicks||0)`)
	if err != nil {
		t.Fatal(err)
	}
	if after.Value.Int() != 1 {
		t.Fatal("activated root did not receive trusted click")
	}
}

func TestLiveLocalDefaultAutoplayAfterInteraction(t *testing.T) {
	loaded := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/watch":
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, `<!doctype html><script>
let requested = false;
const autoplay = () => {
  if (!requested && document.visibilityState === 'visible') {
    requested = true;
    fetch('/episode.mp4');
  }
};
addEventListener('load', () => { fetch('/loaded?visibility='+document.visibilityState); autoplay(); });
document.addEventListener('visibilitychange', autoplay);
</script>`)
		case "/loaded":
			loaded <- req.URL.Query().Get("visibility")
		case "/episode.mp4":
			w.Header().Set("Content-Type", "video/mp4")
			io.WriteString(w, "local media fixture")
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(server.Close)
	eng := localFallbackEngine(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	owner, err := eng.browser.Context(ctx).Page(proto.TargetCreateTarget{URL: server.URL + "/owner"})
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.WaitLoad(); err != nil {
		t.Fatal(err)
	}
	eng.interactions <- struct{}{}
	held := true
	defer func() {
		if held {
			<-eng.interactions
		}
	}()
	var logs outcomeLog
	done := make(chan error, 1)
	go func() {
		found, err := eng.capture(traceCapture(ctx, &logs), Request{PageURL: server.URL + "/watch", Settle: 100 * time.Millisecond, Accept: func(c Candidate) bool { return c.URL == server.URL+"/episode.mp4" }})
		if err == nil && (len(found) != 1 || found[0].Source != "default") {
			t.Error("default autoplay lost provenance")
		}
		done <- err
	}()
	select {
	case visibility := <-loaded:
		if visibility != "hidden" {
			t.Fatal("new navigation stole focus from the active interaction")
		}
	case <-ctx.Done():
		t.Fatal("another interaction blocked navigation")
	}
	<-eng.interactions
	held = false
	if err := <-done; err != nil {
		t.Fatalf("default autoplay did not resume after foreground became available: %v", err)
	}
	assertSourceOutcomes(t, logs.String(), map[string]string{"default": "resolved"})
}
