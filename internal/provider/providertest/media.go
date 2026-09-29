//go:build live

// Package providertest contains opt-in checks of real provider playback.
package providertest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"tarragon-anime/internal/mpv"
	"tarragon-anime/internal/provider"
)

// CheckMedia probes a full episode and decodes one frame through the production
// mpv handoff. It opens no video window or audio device and writes no progress.
func CheckMedia(t *testing.T, ctx context.Context, stream provider.Stream) {
	t.Helper()
	if os.Getenv("ANIME_LIVE_MEDIA") != "1" {
		t.Log("set ANIME_LIVE_MEDIA=1 to verify audio/video and decode a frame")
		return
	}
	var headers strings.Builder
	for key, value := range stream.Headers {
		fmt.Fprintf(&headers, "%s: %s\r\n", key, value)
	}
	probeCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	probe := exec.CommandContext(probeCtx, "ffprobe", "-v", "error",
		"-headers", headers.String(), "-show_entries", "format=duration:stream=codec_type",
		"-of", "json", stream.URL)
	var diagnostics bytes.Buffer
	probe.Stderr = &diagnostics
	output, err := probe.Output()
	if err != nil {
		t.Fatalf("ffprobe failed: %v\n%s", err, diagnostics.String())
	}
	var report struct {
		Streams []struct {
			Type string `json:"codec_type"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(output, &report); err != nil {
		t.Fatal(err)
	}
	video, audio := false, false
	for _, track := range report.Streams {
		video = video || track.Type == "video"
		audio = audio || track.Type == "audio"
	}
	seconds, _ := strconv.ParseFloat(report.Format.Duration, 64)
	if !video || !audio || seconds < 600 {
		t.Fatalf("expected a full episode with audio/video: video=%t audio=%t duration=%.1fs\n%s\n%s", video, audio, seconds, output, diagnostics.String())
	}

	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	player := mpv.New([]string{
		"--no-config", "--vo=null", "--ao=null", "--frames=1", "--load-scripts=no",
		"--ytdl=no", "--network-timeout=10",
	}, log.New(io.Discard, "", 0))
	playCtx, stop := context.WithTimeout(ctx, 45*time.Second)
	defer stop()
	session, err := player.Play(playCtx, mpv.Stream{URL: stream.URL, Headers: stream.Headers, Subtitle: stream.Subtitle}, "Provider playback test", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	loaded := false
	for {
		select {
		case event, ok := <-session.Events():
			if !ok {
				t.Fatal("mpv closed before decoding a frame")
			}
			if event.Type == mpv.EventFileLoaded {
				loaded = true
			}
			if event.Type == mpv.EventEndFile {
				if !loaded || event.Reason != "eof" {
					t.Fatalf("mpv decode failed: loaded=%t reason=%s error=%s", loaded, event.Reason, event.Error)
				}
				t.Logf("verified %.0fs of audio/video and decoded a frame with mpv", seconds)
				return
			}
		case <-playCtx.Done():
			t.Fatal(playCtx.Err())
		}
	}
}
