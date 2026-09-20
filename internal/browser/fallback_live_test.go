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

// These tests use only local fixtures and disposable profiles, never liveOptions.
func localFallbackEngine(t *testing.T) *chromeEngine {
	t.Helper()
	bin, err := FindBinary()
	if err != nil {
		t.Skip(err)
	}
	opts := New(Options{Binary: bin, ProfileDir: t.TempDir(), Headless: true}).opts
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	eng, err := newChromeEngine(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(eng.close)
	return eng.(*chromeEngine)
}

func TestLiveLocalSourceButtonSelection(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, `<!doctype html>
<a class="source">Link</a><div class="source">Player</div>
<button class="source warn">Warning</button>
<button class="source" hidden>Mp4</button>
<button class="source" disabled>Mp4</button>
<fieldset disabled><button class="source">Mp4</button></fieldset>
<button class="source" aria-disabled="true">Mp4</button>
<button class="source" style="visibility:hidden">Mp4</button>
<button class="source"> &#32; </button>
<iframe srcdoc="<button class='source'>Iframe</button>"></iframe>
<button>Outside selector</button>
<button id="active" class="source pair current" aria-selected="true"> Mp4 </button>
<button class="source pair">Mp4</button>
<button id="second" class="source pair">Ok</button>
<button id="third" class="source"> Multi
 Source </button>
<button id="fourth" class="source">Fourth</button>`)
	}))
	t.Cleanup(server.Close)
	eng := localFallbackEngine(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	page, err := eng.browser.Context(ctx).Page(proto.TargetCreateTarget{URL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	if err := page.WaitLoad(); err != nil {
		t.Fatal(err)
	}
	tried := map[string]bool{}
	for _, want := range []struct{ id, label string }{{"active", "Mp4"}, {"second", "Ok"}, {"third", "Multi Source"}} {
		button, label, err := nextSourceButton(page, ".source:not(.warn)", tried)
		if err != nil || button == nil || label != want.label {
			t.Fatalf("nextSourceButton() label=%q err=%v, want %q", label, err, want.label)
		}
		id, err := button.Attribute("id")
		if err != nil || id == nil || *id != want.id {
			t.Fatalf("selected wrong button for %q: id=%v err=%v", label, id, err)
		}
		tried[label] = true
	}
	if button, _, err := nextSourceButton(page, ".source:not(.warn)", tried); err != nil || button != nil {
		t.Fatalf("selected a fourth source: button=%v err=%v", button, err)
	}
	if button, _, err := nextSourceButton(page, ".pair", map[string]bool{"Mp4": true, "Ok": true}); err != nil || button != nil {
		t.Fatalf("reselected a tried label: button=%v err=%v", button, err)
	}
	if button, _, err := nextSourceButton(page, ".absent", map[string]bool{}); err != nil || button != nil {
		t.Fatalf("missing selector did not stop: button=%v err=%v", button, err)
	}
	if _, _, err := nextSourceButton(page, "[", map[string]bool{}); err == nil {
		t.Fatal("invalid selector did not report failure")
	}
	cancel()
	if _, _, err := nextSourceButton(page, ".source", map[string]bool{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("selection ignored cancellation: %v", err)
	}
}

func TestLiveLocalSourceFallbackCapture(t *testing.T) {
	var mu sync.Mutex
	clicks := map[string][]string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/click":
			mu.Lock()
			mode := r.URL.Query().Get("case")
			clicks[mode] = append(clicks[mode], r.URL.Query().Get("label"))
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case "/episode.mp4":
			cookie, err := r.Cookie("session")
			if err != nil || cookie.Value != "local-cookie" || r.Header.Get("Authorization") != "local-credential" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			// A fetch fixture tests capture and wire headers, not video decoding.
			const body = "local media fixture"
			w.Header().Set("Content-Type", "video/mp4")
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", len(body)-1, len(body)))
			w.WriteHeader(http.StatusPartialContent)
			io.WriteString(w, body)
		case "/denied.mp4":
			w.Header().Set("Content-Type", "video/mp4")
			w.WriteHeader(http.StatusForbidden)
		case "/subtitle.vtt":
			w.Header().Set("Content-Type", "text/vtt")
			io.WriteString(w, "WEBVTT\n")
		default:
			if !strings.HasPrefix(r.URL.Path, "/watch/") {
				http.NotFound(w, r)
				return
			}
			mode := strings.TrimPrefix(r.URL.Path, "/watch/")
			w.Header().Set("Content-Type", "text/html")
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "local-cookie", Path: "/", HttpOnly: true})
			if mode == "challenge" {
				w.WriteHeader(http.StatusForbidden)
			}
			fmt.Fprintf(w, `<!doctype html>
<button class="source warn">Warning</button>
<button class="source" hidden>Hidden</button>
<button class="source" disabled>Disabled</button>
<button class="source"> </button>
<button class="source current" aria-selected="true">Mp4</button>
<button class="source"> Mp4 </button>
<button class="source">Ok</button>
<button class="source">Third</button>
<button class="source">Fourth</button>
<button>Outside</button>
<script>
const mode = %q;
const media = () => fetch('/episode.mp4', {headers: {Authorization: 'local-credential'}});
if (mode === 'initial') media();
if (mode === 'next') fetch('/subtitle.vtt');
if (mode === 'blocked') {
  const overlay = document.createElement('div');
  overlay.style = 'position:fixed;inset:0;z-index:10';
  document.body.append(overlay);
}
document.addEventListener('click', event => {
  const label = event.target.textContent.trim();
  if (!event.isTrusted) return;
  fetch('/click?case=' + mode + '&label=' + encodeURIComponent(label));
  if (mode === 'active' && label === 'Mp4' || mode === 'next' && label === 'Ok') media();
  if (mode === 'next' && label === 'Mp4') fetch('/denied.mp4');
});
</script>`, mode)
		}
	}))
	t.Cleanup(server.Close)
	eng := localFallbackEngine(t)
	const selector = "button.source:not(.warn)"
	// Match the production resolver even when go test permits more goroutines.
	sessions := make(chan struct{}, defaultMaxSessions)
	for _, test := range []struct {
		name     string
		selector string
		wantErr  error
		labels   []string
		attempts int
		timeout  time.Duration
	}{
		{name: "active", selector: selector, labels: []string{"Mp4"}, attempts: 1},
		{name: "next", selector: selector, labels: []string{"Mp4", "Ok"}, attempts: 2},
		{name: "exhausted", selector: selector, wantErr: ErrNoMedia, labels: []string{"Mp4", "Ok", "Third"}, attempts: 3},
		{name: "opt-out", wantErr: ErrNoMedia},
		{name: "missing", selector: "button.absent", wantErr: ErrNoMedia},
		{name: "invalid", selector: "[", wantErr: ErrNoMedia},
		{name: "initial", selector: selector},
		{name: "challenge", selector: selector, wantErr: ErrChallenged},
		{name: "blocked", selector: selector, wantErr: context.DeadlineExceeded, attempts: 1, timeout: noMediaGrace + 2*time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			sessions <- struct{}{}
			defer func() { <-sessions }()
			timeout := test.timeout
			if timeout == 0 {
				timeout = 4*noMediaGrace + 5*time.Second
			}
			ctx, cancel := context.WithTimeout(t.Context(), timeout)
			defer cancel()
			var outcomes outcomeLog
			ctx = traceCapture(ctx, &outcomes)
			var logs bytes.Buffer
			chrome := *eng
			chrome.logger = log.New(&logs, "", 0)
			start := time.Now()
			found, err := chrome.capture(ctx, Request{
				PageURL: server.URL + "/watch/" + test.name, FallbackSelector: test.selector, Settle: 100 * time.Millisecond,
				Accept: func(c Candidate) bool {
					return c.URL == server.URL+"/episode.mp4" || c.URL == server.URL+"/denied.mp4" || c.URL == server.URL+"/subtitle.vtt"
				},
				Ready: func(c Candidate) bool { return c.MIME == "video/mp4" },
			})
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("capture() = %v, want %v", err, test.wantErr)
			}
			wantOutcomes := map[string]string{}
			for i := 0; i <= test.attempts; i++ {
				source := "default"
				if i > 0 {
					source = fmt.Sprintf("source-%d", i)
				}
				wantOutcomes[source] = "unavailable"
				if i == test.attempts {
					status, _ := Outcome(ctx, err)
					wantOutcomes[source] = status
				}
			}
			assertSourceOutcomes(t, outcomes.String(), wantOutcomes)
			for _, secret := range []string{server.URL, "local-cookie", "local-credential", "Mp4", "Ok", "Third"} {
				if strings.Contains(outcomes.String(), secret) {
					t.Fatalf("outcome leaked %q", secret)
				}
			}
			if test.wantErr != nil {
				if len(found) != 0 {
					t.Fatalf("failed capture returned candidates: %#v", found)
				}
			} else {
				wantSource := "default"
				if test.attempts > 0 {
					wantSource = fmt.Sprintf("source-%d", test.attempts)
				}
				if len(found) != 1 || found[0].Source != wantSource {
					t.Fatalf("source provenance = %#v", found)
				}
				if len(found) != 1 || found[0].URL != server.URL+"/episode.mp4" || found[0].Status != http.StatusPartialContent {
					t.Fatalf("capture did not return only fresh HTTP 206 media: %#v", found)
				}
				headers := http.Header{}
				for key, value := range found[0].Headers {
					headers.Set(key, value)
				}
				if !strings.Contains(headers.Get("Cookie"), "session=local-cookie") || headers.Get("Authorization") != "local-credential" {
					t.Fatal("fresh source media lost its wire credentials")
				}
				if found[0].Observed > 5*time.Second {
					t.Fatal("source media retained the old observation start")
				}
			}
			mu.Lock()
			got := slices.Clone(clicks[test.name])
			mu.Unlock()
			if !slices.Equal(got, test.labels) {
				t.Fatalf("trusted clicks = %v, want %v", got, test.labels)
			}
			var wantLog strings.Builder
			for attempt := 1; attempt <= test.attempts; attempt++ {
				fmt.Fprintf(&wantLog, "browser source fallback attempt=%d\n", attempt)
			}
			if logs.String() != wantLog.String() {
				t.Fatalf("fallback must log only attempt counters: %q", logs.String())
			}
			limit := time.Duration(test.attempts+1)*noMediaGrace + 4*time.Second
			if test.name == "blocked" {
				limit = timeout + 2*time.Second
			}
			if elapsed := time.Since(start); elapsed > limit {
				t.Fatalf("capture did not stop boundedly: %s > %s", elapsed, limit)
			}
		})
	}
}
