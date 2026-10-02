package anipub

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tarragon-anime/internal/browser"
)

func encryptFixture(t *testing.T, codec *sourceCipher, value string) string {
	t.Helper()
	block, err := aes.NewCipher(codec.key[:])
	if err != nil {
		t.Fatal(err)
	}
	padding := aes.BlockSize - len(value)%aes.BlockSize
	body := append([]byte(value), bytes.Repeat([]byte{byte(padding)}, padding)...)
	cipher.NewCBCEncrypter(block, codec.iv[:]).CryptBlocks(body, body)
	return base64.RawURLEncoding.EncodeToString(body)
}

func TestNativeStreamsQualityTranslationAndPlaybackLifetime(t *testing.T) {
	for _, current := range []bool{false, true} {
		t.Run(fmt.Sprintf("current=%t", current), func(t *testing.T) {
			testNativeStreamsQualityTranslationAndPlaybackLifetime(t, current)
		})
	}
}

func testNativeStreamsQualityTranslationAndPlaybackLifetime(t *testing.T, current bool) {
	t.Helper()
	var codec sourceCipher
	copy(codec.key[:], "fixture-key")
	copy(codec.iv[:], "0123456789abcdef")
	var sources atomic.Int32
	payload := bytes.Repeat([]byte{0x47}, 188*3)
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/play/59970/1/sub", "/play/59970/1/dub":
			mode := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			fmt.Fprintf(w, `<iframe src=/embed/%s></iframe>`, mode)
		case "/embed/sub", "/embed/dub":
			id := "101"
			if r.URL.Path == "/embed/dub" {
				id = "102"
			}
			fmt.Fprintf(w, `<script src="/lib/newclient.min.js?v=rotating"></script><div class="player" id="megaplay-player" data-id="%s"></div>`, id)
		case "/lib/newclient.min.js":
			if current {
				fmt.Fprint(w, `old="stream/getSources"; next="stream/getSourcesNew"; mode="AES-CBC";
					key=String(e.pick(["trustAesKey","TRUST_AES_KEY"],"fixture-key"));
					iv=String(e.pick(["trustAesIv","TRUST_AES_IV"],"0123456789abcdef"));
					noise=new TextEncoder().encode(String(t));`)
			} else {
				fmt.Fprint(w, `key=(new TextEncoder).encode("fixture-key"); mode="AES-CBC"; iv=(new TextEncoder).encode("0123456789abcdef")`)
			}
		case "/stream/getSources", "/stream/getSourcesNew":
			if (r.URL.Path == "/stream/getSourcesNew") != current {
				t.Errorf("wrong source endpoint for current=%t: %s", current, r.URL.Path)
			}
			sources.Add(1)
			id := r.URL.Query().Get("id")
			if id != "101" && id != "102" || r.Header.Get("X-Requested-With") != "XMLHttpRequest" {
				t.Errorf("wrong source request: %s", r.URL)
			}
			file, _ := json.Marshal(map[string]string{"file": server.URL + "/media/" + id + "/master.m3u8"})
			json.NewEncoder(w).Encode(map[string]any{
				"enc":    encryptFixture(t, &codec, string(file)),
				"tracks": []map[string]string{{"file": server.URL + "/subs.vtt", "label": "English", "kind": "captions"}},
			})
		case "/media/101/master.m3u8", "/media/102/master.m3u8":
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-STREAM-INF:RESOLUTION=1920x1080\n1080.m3u8\n#EXT-X-STREAM-INF:RESOLUTION=1280x720\n720.m3u8\n#EXT-X-STREAM-INF:RESOLUTION=640x360\n360.m3u8\n")
		case "/media/101/720.m3u8", "/media/101/1080.m3u8", "/media/102/360.m3u8":
			// Test the encrypted segment form as well as the live plaintext form.
			token := encryptFixture(t, &codec, server.URL+"/segment.jpg?quality="+url.QueryEscape(r.URL.Path))
			fmt.Fprintf(w, "#EXTM3U\n#EXT-X-TARGETDURATION:10\n#EXTINF:10,\n/segment/%s\n#EXT-X-ENDLIST\n", token)
		case "/segment.jpg":
			if r.Header.Get("Referer") != server.URL+"/" || r.Header.Get("User-Agent") != userAgent {
				t.Errorf("playback context missing: %v", r.Header)
			}
			if r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
				t.Error("playback leaked credentials")
			}
			w.Header().Set("Content-Type", "image/jpeg")
			http.ServeContent(w, r, "segment.jpg", time.Time{}, bytes.NewReader(payload))
		default:
			t.Errorf("unexpected request %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	httpClient := server.Client()
	httpClient.Jar, _ = cookiejar.New(nil)
	origin, _ := url.Parse(server.URL)
	httpClient.Jar.SetCookies(origin, []*http.Cookie{{Name: "player", Value: "private", Path: "/"}})
	c := NewClientWithBaseURL(httpClient, server.URL)
	defer c.Close()
	c.UseBrowser(captureFunc(func(context.Context, browser.Request) ([]browser.Candidate, error) {
		t.Error("working native source launched Chromium")
		return nil, errors.New("unexpected browser")
	}))
	ep := Episode{ShowID: "59970", Number: 1}
	for _, test := range []struct{ mode, quality, want string }{
		{"sub", "720p", "/media/101/720.m3u8"},
		{"sub", "best", "/media/101/1080.m3u8"},
		{"dub", "worst", "/media/102/360.m3u8"},
	} {
		t.Run(test.mode+"/"+test.quality, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			streams, err := c.Streams(ctx, ep, test.mode, test.quality)
			if err != nil || len(streams) != 1 {
				t.Fatalf("Streams: %v, %v", streams, err)
			}
			cancel() // The service cancels all lookup contexts after selecting a winner.
			stream := streams[0]
			if stream.Source != "native" || (stream.Subtitle != "") != (test.mode == "sub") {
				t.Fatalf("incorrect translation or source: %+v", stream)
			}
			resp, err := http.Get(stream.URL)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if !strings.Contains(string(body), "0.ts") || strings.Contains(string(body), "segment/") {
				t.Fatalf("playlist wasn't normalized: %s", body)
			}
			segment, _ := resourceURL(stream.URL, "0.ts")
			req, _ := http.NewRequest(http.MethodGet, segment, nil)
			req.Header.Set("Range", "bytes=0-187")
			req.Header.Set("Cookie", "unrelated=private")
			resp, err = http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, _ = io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusPartialContent || resp.Header.Get("Content-Type") != "video/mp2t" || !bytes.Equal(body, payload[:188]) {
				t.Fatalf("segment: status=%d headers=%v bytes=%d", resp.StatusCode, resp.Header, len(body))
			}
			c.proxy.mu.RLock()
			for _, entry := range c.proxy.entries {
				if strings.Contains(entry.resources["0.ts"], url.QueryEscape(test.want)) {
					c.proxy.mu.RUnlock()
					before := sources.Load()
					cached, err := c.Streams(t.Context(), ep, test.mode, test.quality)
					if err != nil || cached[0].URL != stream.URL || before != sources.Load() {
						t.Fatalf("native stream cache miss: %v", err)
					}
					c.InvalidateStreams(ep, test.mode, test.quality)
					if _, ok := c.cachedStreams(ep, test.mode, test.quality); ok {
						t.Fatal("invalidation missed the default episode value")
					}
					return
				}
			}
			c.proxy.mu.RUnlock()
			t.Fatalf("wrong rendition selected, want %s", test.want)
		})
	}
}

func TestSourceCipherRejectsDriftAndMalformedTokens(t *testing.T) {
	for _, script := range []string{"", `"AES-CBC"; x.encode("key"); x.encode("short")`, `"AES-CBC"; x.encode("key"); x.encode("0123456789abcdef"); x.encode("extra")`} {
		if _, err := parseSourceCipher(script); err == nil {
			t.Fatalf("accepted unsupported cipher: %s", script)
		}
	}
	codec, err := parseSourceCipher(`"AES-CBC"; x.encode("rotated-key"); x.encode("0123456789abcdef")`)
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"", "not+base64", "YQ", strings.Repeat("A", 22)} {
		if _, err := codec.decrypt(token); err == nil {
			t.Fatalf("accepted malformed token %q", token)
		}
	}
	plain, err := codec.decrypt(encryptFixture(t, codec, "https://cdn.test/rotated.m3u8"))
	if err != nil || string(plain) != "https://cdn.test/rotated.m3u8" {
		t.Fatalf("rotated cipher: %q, %v", plain, err)
	}
}

func TestCurrentSourceCipherRotatesAndRejectsAmbiguousDefaults(t *testing.T) {
	script := `rewrite="stream/getSourcesNew"; mode="AES-CBC";
		key=e.pick(["trustAesKey", "TRUST_AES_KEY"], "rotated\\key");
		iv=e.pick(["trustAesIv", "TRUST_AES_IV"], "0123456789abcdef");
		new TextEncoder().encode("unrelated");`
	codec, err := parseSourceCipher(script)
	if err != nil {
		t.Fatal(err)
	}
	if codec.sourcesPath != "/stream/getSourcesNew" {
		t.Fatalf("source endpoint = %s", codec.sourcesPath)
	}
	var expected sourceCipher
	copy(expected.key[:], `rotated\key`)
	copy(expected.iv[:], "0123456789abcdef")
	plain, err := codec.decrypt(encryptFixture(t, &expected, `{"file":"https://cdn.test/rotated.m3u8"}`))
	if err != nil || string(plain) != `{"file":"https://cdn.test/rotated.m3u8"}` {
		t.Fatalf("rotated named cipher: %q, %v", plain, err)
	}
	for _, invalid := range []string{
		strings.ReplaceAll(script, "AES-CBC", "AES-GCM"),
		strings.ReplaceAll(script, "trustAesKey", "unrecognizedKey"),
		strings.ReplaceAll(script, "trustAesIv", "unrecognizedIV"),
		strings.ReplaceAll(script, "0123456789abcdef", "short"),
		strings.ReplaceAll(script, `rotated\\key`, strings.Repeat("a", 33)),
		script + `other=e.pick(["trustAesKey","TRUST_AES_KEY"],"ambiguous");`,
		script + `other=e.pick(["trustAesIv","TRUST_AES_IV"],"fedcba9876543210");`,
	} {
		if _, err := parseSourceCipher(invalid); err == nil {
			t.Fatalf("accepted unsupported named cipher: %s", invalid)
		}
	}
}

func TestNativeSourceCipherRefusesPageOverrides(t *testing.T) {
	for _, name := range []string{"MegaPlayClientConfig", "MEGAPLAY_CLIENT_CONFIG"} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/play/61316/19/sub":
					io.WriteString(w, `<iframe src="/embed"></iframe>`)
				case "/embed":
					fmt.Fprintf(w, `<script>window.%s={trustAesKey:"override"};</script><script src="/lib/newclient.min.js"></script><div id="megaplay-player" data-id="179970"></div>`, name)
				default:
					t.Errorf("unexpected request after cipher override: %s", r.URL)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			c := NewClientWithBaseURL(server.Client(), server.URL)
			defer c.Close()
			_, err := c.Streams(t.Context(), Episode{ShowID: "61316", Number: 19}, "sub", "best")
			if err == nil || !strings.Contains(err.Error(), "configuration override") {
				t.Fatalf("ignored page cipher override: %v", err)
			}
		})
	}
}

func TestPlaylistRewritesKeysMapsAndRejectsUnsupportedMedia(t *testing.T) {
	playlist := "#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"key.bin\"\n#EXT-X-MAP:URI=\"init.mp4\"\n#EXTINF:6,\nsegment.jpg\n#EXT-X-ENDLIST\n"
	body, resources, err := rewriteMediaPlaylist("https://cdn.test/media/index.m3u8", playlist, nil)
	if err != nil || resources["0.key"] != "https://cdn.test/media/key.bin" || resources["1.mp4"] != "https://cdn.test/media/init.mp4" || resources["2.m4s"] != "https://cdn.test/media/segment.jpg" {
		t.Fatalf("rewritten resources: %v, %v", resources, err)
	}
	if !strings.Contains(body, `URI="0.key"`) || !strings.Contains(body, `URI="1.mp4"`) {
		t.Fatalf("missing URI attributes: %s", body)
	}
	for _, invalid := range []string{
		"<html>temporarily unavailable</html>",
		"#EXTM3U\n#EXTINF:5,\nseg.jpg\n", // Live playlists must not be frozen.
		"#EXTM3U\n#EXT-X-ENDLIST\n",
		"#EXTM3U\n#EXTINF:5,\n/segment/encrypted\n#EXT-X-ENDLIST\n",
		"#EXTM3U\n#EXTINF:5,\nfile:///etc/passwd\n#EXT-X-ENDLIST\n",
	} {
		if _, _, err := rewriteMediaPlaylist("https://cdn.test/index.m3u8", invalid, nil); err == nil {
			t.Fatalf("accepted invalid media: %q", invalid)
		}
	}
	if _, err := selectPlaylist("https://cdn.test/master.m3u8", "#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,URI=\"audio.m3u8\"", "best"); err == nil {
		t.Fatal("silently dropped a separate audio rendition")
	}
}

func TestProxyBoundsCancellationAndClose(t *testing.T) {
	c := NewClient(nil)
	defer c.Close()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := c.publish(ctx, "playlist", nil, nil); !errors.Is(err, context.Canceled) || c.proxy != nil {
		t.Fatalf("cancelled publication created a proxy: %v", err)
	}
	var first, last string
	for i := 0; i <= maxProxyEntries; i++ {
		var err error
		last, err = c.publish(t.Context(), "playlist", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = last
		}
	}
	for _, test := range []struct {
		url    string
		status int
	}{{first, 404}, {last, 200}, {strings.TrimSuffix(last, "stream.m3u8") + "unknown.ts", 404}} {
		resp, err := http.Get(test.url)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != test.status {
			t.Fatalf("status=%d want=%d", resp.StatusCode, test.status)
		}
	}
	c.Close()
	if _, err := c.publish(t.Context(), "playlist", nil, nil); err == nil {
		t.Fatal("closed client reopened the proxy")
	}
}
