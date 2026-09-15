//go:build live

package animeflix

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestLiveMyStream(t *testing.T) {
	testLiveMyStream(t, 1735, []string{"Naruto: Shippuden", "Naruto Shippuden"}, 1)
}

func TestLiveMyStreamSlimeSeason4(t *testing.T) {
	testLiveMyStream(t, 182205, []string{"That Time I Got Reincarnated as a Slime Season 4"}, 5)
}

func testLiveMyStream(t *testing.T, mediaID int, aliases []string, number int) {
	t.Helper()
	client := NewClient(&http.Client{Timeout: 20 * time.Second})
	defer client.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	anime, err := client.Match(ctx, mediaID, aliases, "sub")
	if err != nil {
		t.Fatal(err)
	}
	episodes, err := client.Episodes(ctx, anime, "sub")
	if err != nil || len(episodes) == 0 {
		t.Fatalf("Episodes: count=%d error=%v", len(episodes), err)
	}
	t.Logf("matched %s, %d episodes", anime.ID, len(episodes))
	index := -1
	for i, episode := range episodes {
		if episode.Number == number {
			index = i
			break
		}
	}
	if index < 0 {
		t.Fatalf("episode %d is unavailable", number)
	}
	for _, mode := range []string{"sub", "dub"} {
		t.Run(mode, func(t *testing.T) {
			streams, err := client.Streams(ctx, episodes[index], mode, "worst")
			if err != nil || len(streams) != 1 {
				t.Fatalf("Streams: count=%d error=%v", len(streams), err)
			}
			stream := streams[0]
			if (stream.Subtitle != "") != (mode == "sub") {
				t.Fatal("incorrect subtitle selection")
			}
			body, _, err := fetch(ctx, http.DefaultClient, http.MethodGet, stream.URL, "", nil)
			if err != nil || strings.Count(string(body), "#EXT-X-MEDIA:") != 1 {
				t.Fatalf("local master: %v", err)
			}
			if os.Getenv("ANIME_LIVE_MEDIA") != "1" {
				return
			}
			// Opt-in, headless decode of one video frame. No window or audio
			// device is opened; this catches FFmpeg extension/MIME restrictions.
			probeCtx, stop := context.WithTimeout(ctx, 45*time.Second)
			defer stop()
			args := []string{"--no-config", "--vo=null", "--ao=null", "--frames=1", "--network-timeout=15", "--msg-level=all=warn,cplayer=info", "--http-header-fields=Referer: " + stream.Headers["Referer"], "--user-agent=" + userAgent}
			if stream.Subtitle != "" {
				args = append(args, "--sub-file="+stream.Subtitle)
			}
			args = append(args, "--", stream.URL)
			output, err := exec.CommandContext(probeCtx, "mpv", args...).CombinedOutput()
			if err != nil {
				t.Fatalf("mpv decode failed: %v\n%s", err, output)
			}
			language := "jpn"
			if mode == "dub" {
				language = "eng"
			}
			if !strings.Contains(string(output), "--alang="+language) || !strings.Contains(string(output), "AO: [null]") || !strings.Contains(string(output), "VO: [null]") {
				t.Fatalf("mpv did not select %s audio: %s", language, output)
			}
			if mode == "sub" && !strings.Contains(string(output), "(webvtt) [external]") {
				t.Fatalf("mpv did not load WebVTT subtitles: %s", output)
			}
			t.Logf("mpv decoded a video frame with %s audio; subtitles=%t", language, mode == "sub")
		})
	}
}
