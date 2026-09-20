//go:build live

package browser

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-rod/rod/lib/proto"
)

func TestLiveLocalPlayButton(t *testing.T) {
	var mu sync.Mutex
	clicks := map[string][]string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mode, source := r.URL.Query().Get("case"), r.URL.Query().Get("source")
		if source == "" {
			source = "initial"
		}
		switch r.URL.Path {
		case "/click":
			mu.Lock()
			clicks[mode] = append(clicks[mode], r.URL.Query().Get("label"))
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
			return
		case "/episode.mp4":
			cookie, err := r.Cookie("session")
			if err != nil || cookie.Value != "local-cookie" || r.Header.Get("Authorization") != "local-credential" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			w.Header().Set("Content-Type", "video/mp4")
			w.WriteHeader(http.StatusPartialContent)
			io.WriteString(w, "local media fixture")
			return
		case "/watch", "/middle", "/third", "/player":
		default:
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		if r.URL.Path == "/watch" {
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "local-cookie", Path: "/", HttpOnly: true})
			if mode == "challenge" {
				w.WriteHeader(http.StatusForbidden)
			}
		}
		fmt.Fprintf(w, `<!doctype html><style>iframe{width:600px;height:350px}button{min-width:40px;min-height:30px}</style>
<script>
const mode = %q, source = %q;
const mark = label => fetch('/click?case=' + mode + '&label=' + encodeURIComponent(label));
const media = () => fetch('/episode.mp4', {headers: {Authorization: 'local-credential'}});
</script>`, mode, source)
		switch r.URL.Path {
		case "/watch":
			// These would qualify if lookup improperly fell back to another frame.
			io.WriteString(w, `<video hidden></video><button class="play" onclick="if(event.isTrusted)mark('outer-ad')">Ad</button>`)
			hidden := ""
			if mode == "hidden" {
				hidden = "hidden"
			}
			fmt.Fprintf(w, `<iframe id="episode" title="Episode player" %s src="/middle?case=%s"></iframe>`, hidden, mode)
			if mode == "ambiguous-root" {
				fmt.Fprintf(w, `<iframe title="Episode player" src="/middle?case=%s"></iframe>`, mode)
			}
			if mode == "nested" || mode == "source" || mode == "retry" {
				label := "Mp4"
				if mode == "retry" {
					label = "Ok"
				}
				fmt.Fprintf(w, `<button class="source" onclick="if(event.isTrusted){mark('source:%s');document.querySelector('#episode').src='/middle?case=%s&source=%s'}">%s</button>`, label, mode, label, label)
			}
		case "/middle":
			io.WriteString(w, `<video hidden></video><button class="play" onclick="if(event.isTrusted)mark('middle-ad')">Ad</button>`)
			if mode == "wrong-node" {
				io.WriteString(w, `<div class="embed"></div>`)
				return
			}
			class, target := "embed", "/player"
			if mode == "missing" || mode == "source" && source == "initial" {
				class = "unselected"
			}
			if mode == "three" {
				target = "/third"
			}
			fmt.Fprintf(w, `<iframe class="%s" src="%s?case=%s&source=%s"></iframe>`, class, target, mode, source)
			if mode == "ambiguous" {
				fmt.Fprintf(w, `<iframe class="embed" src="/player?case=%s"></iframe>`, mode)
			}
		case "/third":
			fmt.Fprintf(w, `<iframe class="inner" src="/player?case=%s"></iframe>`, mode)
		case "/player":
			switch mode {
			case "no-video":
				io.WriteString(w, `<div class="vjs-tech"></div>`)
			case "svg":
				io.WriteString(w, `<svg><video></video></svg>`)
			case "multi-video":
				io.WriteString(w, `<video muted></video><video muted></video>`)
			default:
				io.WriteString(w, `<video class="vjs-tech" muted style="width:160px;height:90px"></video>`)
			}
			io.WriteString(w, `<a class="play">Not a button</a>
<button class="play" hidden>Hidden</button><button class="play" disabled>Disabled</button>
<button class="play" aria-disabled="true">Disabled</button>
<button class="play" style="visibility:hidden">Hidden</button>`)
			class := "play"
			if mode == "no-button" {
				class = "outside-selector"
			}
			fmt.Fprintf(w, `<button id="start" class="%s" aria-label="Play"></button>`, class)
			io.WriteString(w, `<script>
const video = document.querySelector('video');
if (mode === 'playing') {
  const canvas = document.createElement('canvas');
  canvas.getContext('2d').fillRect(0, 0, 10, 10);
  video.srcObject = canvas.captureStream(1);
  video.play();
}
if (mode === 'initial') media();
if (mode === 'blocked') {
  const overlay = document.createElement('div');
  overlay.style = 'position:fixed;inset:0;z-index:10';
  document.body.append(overlay);
}
document.addEventListener('click', event => {
  if (!event.isTrusted) return;
  if (event.target.id !== 'start') { mark('wrong-button'); return; }
  if (!(video instanceof HTMLVideoElement) || !video.paused || video.readyState !== 0) {
    mark('wrong-video-state'); return;
  }
  mark('play:' + source);
  if (mode === 'nested' || mode === 'source' && source === 'Mp4' || mode === 'retry' && source === 'Ok') media();
});
</script>`)
		}
	}))
	t.Cleanup(server.Close)
	path := []string{`iframe[title="Episode player"]`, "iframe.embed", ".play"}
	t.Run("selection", func(t *testing.T) {
		eng := localFallbackEngine(t)
		for _, mode := range []string{"nested", "three", "playing", "missing", "ambiguous", "ambiguous-root", "hidden", "wrong-node", "no-video", "multi-video", "svg", "no-button"} {
			t.Run(mode, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				page, err := eng.browser.Context(ctx).Page(proto.TargetCreateTarget{URL: server.URL + "/watch?case=" + mode})
				if err != nil {
					t.Fatal(err)
				}
				defer page.Close()
				if err := page.WaitLoad(); err != nil {
					t.Fatal(err)
				}
				selectedPath := path
				if mode == "three" {
					selectedPath = []string{path[0], path[1], "iframe.inner", path[2]}
				}
				button, err := findPlayButton(page, selectedPath)
				want := mode == "nested" || mode == "three"
				if err != nil || (button != nil) != want {
					t.Fatalf("findPlayButton() = %v, %v; want button=%t", button, err, want)
				}
				if button != nil {
					id, err := button.Attribute("id")
					if err != nil || id == nil || *id != "start" {
						t.Fatalf("selected wrong play button: id=%v err=%v", id, err)
					}
				}
				cancelled, stop := context.WithCancel(ctx)
				stop()
				if _, err := findPlayButton(page.Context(cancelled), selectedPath); !errors.Is(err, context.Canceled) {
					t.Fatalf("play lookup ignored cancellation: %v", err)
				}
			})
		}
	})
	const startLog = "browser player start\n"
	const sourceLog = "browser source fallback attempt=1\n"
	// Source and iframe-play interactions must work in a shared browser, not
	// just in isolated per-capture processes. Keep the production two-slot bound.
	eng := localFallbackEngine(t)
	sessions := make(chan struct{}, defaultMaxSessions)
	for _, test := range []struct {
		name       string
		wantErr    error
		clicks     []string
		log        string
		graces     int
		withSource bool
	}{
		{name: "nested", clicks: []string{"play:initial"}, log: startLog, graces: 1, withSource: true},
		{name: "source", clicks: []string{"source:Mp4", "play:Mp4"}, log: sourceLog + startLog, graces: 2, withSource: true},
		{name: "retry", clicks: []string{"play:initial", "source:Ok", "play:Ok"}, log: startLog + sourceLog + startLog, graces: 3, withSource: true},
		{name: "once", wantErr: ErrNoMedia, clicks: []string{"play:initial"}, log: startLog, graces: 2},
		{name: "missing", wantErr: ErrNoMedia, graces: 1},
		{name: "ambiguous", wantErr: ErrNoMedia, graces: 1},
		{name: "no-button", wantErr: ErrNoMedia, graces: 1},
		{name: "playing", wantErr: ErrNoMedia, graces: 1},
		{name: "opt-out", wantErr: ErrNoMedia, graces: 1},
		{name: "blocked", wantErr: context.DeadlineExceeded, log: startLog, graces: 1},
		{name: "initial"},
		{name: "challenge", wantErr: ErrChallenged},
	} {
		t.Run("capture/"+test.name, func(t *testing.T) {
			t.Parallel()
			sessions <- struct{}{}
			defer func() { <-sessions }()
			timeout := defaultTimeout
			if test.name == "blocked" {
				timeout = noMediaGrace + 2*time.Second
			}
			ctx, cancel := context.WithTimeout(t.Context(), timeout)
			defer cancel()
			var outcomes outcomeLog
			ctx = traceCapture(ctx, &outcomes)
			var logs bytes.Buffer
			chrome := *eng
			chrome.logger = log.New(&logs, "", 0)
			req := Request{
				PageURL: server.URL + "/watch?case=" + test.name, PlayButtonPath: path,
				Accept: func(c Candidate) bool { return c.URL == server.URL+"/episode.mp4" },
				Settle: 100 * time.Millisecond,
			}
			if test.name == "opt-out" {
				req.PlayButtonPath = nil
			}
			if test.withSource {
				req.FallbackSelector = "button.source"
			}
			start := time.Now()
			found, err := chrome.capture(ctx, req)
			status, _ := Outcome(ctx, err)
			wantOutcomes := map[string]string{"default": status}
			if test.name == "source" || test.name == "retry" {
				wantOutcomes["default"] = "unavailable"
				wantOutcomes["source-1"] = status
			}
			assertSourceOutcomes(t, outcomes.String(), wantOutcomes)
			if got := strings.Count(outcomes.String(), "stage=browser_play status=started"); got != strings.Count(test.log, startLog) {
				t.Fatalf("play stages=%d; logs=%s", got, outcomes.String())
			}
			wantDispatched := strings.Count(test.log, startLog)
			if test.name == "blocked" {
				wantDispatched = 0
			}
			if got := strings.Count(outcomes.String(), "stage=browser_play status=resolved reason=click_dispatched "); got != wantDispatched {
				t.Fatalf("dispatched play outcomes=%d, want %d; logs=%s", got, wantDispatched, outcomes.String())
			}
			mu.Lock()
			got := slices.Clone(clicks[test.name])
			mu.Unlock()
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("capture() = %v, want %v; fixture clicks=%v logs=%q", err, test.wantErr, got, logs.String())
			}
			if test.wantErr != nil {
				if len(found) != 0 {
					t.Fatalf("failed capture returned candidates: %#v", found)
				}
			} else {
				wantSource := "default"
				if test.name == "source" || test.name == "retry" {
					wantSource = "source-1"
				}
				if len(found) != 1 || found[0].Source != wantSource {
					t.Fatalf("source provenance = %#v", found)
				}
				if len(found) != 1 || found[0].URL != server.URL+"/episode.mp4" || found[0].Status != http.StatusPartialContent || found[0].Frame == "" {
					t.Fatalf("missing HTTP 206 media from the player iframe: %#v", found)
				}
				if found[0].Observed > 5*time.Second {
					t.Fatal("play did not reset the collector before the immediate request")
				}
				headers := http.Header{}
				for key, value := range found[0].Headers {
					headers.Set(key, value)
				}
				if headers.Get("Authorization") != "local-credential" || !strings.Contains(headers.Get("Cookie"), "session=local-cookie") {
					t.Fatal("player media lost wire credentials")
				}
			}
			if !slices.Equal(got, test.clicks) {
				t.Fatalf("trusted clicks = %v, want %v", got, test.clicks)
			}
			if logs.String() != test.log {
				t.Fatalf("unexpected player logs: %q, want %q", logs.String(), test.log)
			}
			if elapsed := time.Since(start); elapsed > time.Duration(test.graces)*noMediaGrace+4*time.Second {
				t.Fatalf("capture exceeded expected grace/deadline bound: %s", elapsed)
			}
		})
	}
}
