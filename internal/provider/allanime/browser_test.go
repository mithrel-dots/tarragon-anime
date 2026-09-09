package allanime

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"tarragon-anime/internal/browser"
)

// captureFunc adapts a function to the Resolver interface so a test can decide
// exactly what a browser session would have observed.
type captureFunc func(context.Context, browser.Request) ([]browser.Candidate, error)

func (f captureFunc) Capture(ctx context.Context, req browser.Request) ([]browser.Candidate, error) {
	return f(ctx, req)
}

func testEpisode() Episode {
	return Episode{ShowID: "ReHMC7TQnch3C6z8j", Number: 1, Value: "1"}
}

// playedCandidate mirrors the request Chromium makes for the episode the site
// actually plays.
func playedCandidate() browser.Candidate {
	return browser.Candidate{
		URL:    "https://tools.fast4speed.rsvp/media9/videos/ReHMC7TQnch3C6z8j/sub/1?Authorization=token",
		Kind:   "Media",
		Status: 206,
		Headers: map[string]string{
			"Referer":         "https://player.test/player.html?id=abc",
			"User-Agent":      browser.UserAgent,
			"Accept-Encoding": "identity;q=1, *;q=0",
			"Range":           "bytes=0-",
		},
		Observed: 900 * time.Millisecond,
	}
}

func TestWatchURL(t *testing.T) {
	got := watchURL("https://mkissa.to/", "ReHMC7TQnch3C6z8j", "12", "DUB")
	want := "https://mkissa.to/anime/ReHMC7TQnch3C6z8j/p-12-dub"
	if got != want {
		t.Fatalf("watchURL() = %q, want %q", got, want)
	}
}

func TestAcceptMediaSelectsTheEpisodeMedia(t *testing.T) {
	accept := acceptMedia("ReHMC7TQnch3C6z8j", "1", "sub")
	cases := []struct {
		name      string
		candidate browser.Candidate
		want      bool
	}{
		{"played media", playedCandidate(), true},
		{"hls manifest", browser.Candidate{URL: "https://cdn.test/hls/master.m3u8", Kind: "XHR"}, true},
		{"subtitle track", browser.Candidate{URL: "https://cdn.test/subs/en.vtt", Kind: "Fetch"}, true},
		{"hls segment", browser.Candidate{URL: "https://cdn.test/hls/seg-12.ts", Kind: "XHR"}, false},
		{"dash segment", browser.Candidate{URL: "https://cdn.test/dash/chunk-3.m4s", Kind: "XHR"}, false},
		{"blob url", browser.Candidate{URL: "blob:https://mkissa.to/9f92-4a86", Kind: "Media"}, false},
		{"data url", browser.Candidate{URL: "data:image/gif;base64,R0lGODlh", Kind: "Image"}, false},
		{"ad video", browser.Candidate{URL: "https://s0.2mdn.net/creative/spot.mp4", Kind: "Media"}, false},
		{"analytics beacon", browser.Candidate{URL: "https://region1.google-analytics.com/g/collect", Kind: "XHR"}, false},
		{"page asset", browser.Candidate{URL: "https://cdn.test/covers/001.webp", Kind: "Image"}, false},
		{"unrelated episode", browser.Candidate{URL: "https://cdn.test/videos/ReHMC7TQnch3C6z8j/sub/7.mp4", Kind: "XHR"}, false},
		{"same episode by path", browser.Candidate{URL: "https://cdn.test/videos/ReHMC7TQnch3C6z8j/sub/1.mp4", Kind: "XHR"}, true},
		{"other show", browser.Candidate{URL: "https://cdn.test/videos/otherShowId/sub/1.mp4", Kind: "XHR"}, false},
		{"other translation", browser.Candidate{URL: "https://cdn.test/videos/ReHMC7TQnch3C6z8j/dub/1.mp4", Kind: "XHR"}, false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := accept(test.candidate); got != test.want {
				t.Fatalf("acceptMedia()(%q) = %t, want %t", test.candidate.URL, got, test.want)
			}
		})
	}
}

func TestStreamsFromCandidatesPrefersThePlayedEpisode(t *testing.T) {
	candidates := []browser.Candidate{
		{URL: "https://cdn.test/videos/otherShowId/sub/1.mp4", Kind: "XHR", Observed: time.Millisecond},
		playedCandidate(),
	}
	streams, err := streamsFromCandidates(candidates, testEpisode(), "sub", "https://mkissa.to", "best")
	if err != nil {
		t.Fatalf("streamsFromCandidates() error = %v", err)
	}
	if streams[0].URL != playedCandidate().URL {
		t.Fatalf("first stream = %q, want the media the player loaded", streams[0].URL)
	}
}

func TestStreamsFromCandidatesHonoursQuality(t *testing.T) {
	candidates := []browser.Candidate{
		{URL: "https://cdn.test/videos/ReHMC7TQnch3C6z8j/sub/1/480p.m3u8", Kind: "XHR"},
		{URL: "https://cdn.test/videos/ReHMC7TQnch3C6z8j/sub/1/1080p.m3u8", Kind: "XHR"},
		{URL: "https://cdn.test/videos/ReHMC7TQnch3C6z8j/sub/1/720p.m3u8", Kind: "XHR"},
	}
	best, err := streamsFromCandidates(candidates, testEpisode(), "sub", "https://mkissa.to", "best")
	if err != nil {
		t.Fatalf("streamsFromCandidates() error = %v", err)
	}
	if best[0].URL != candidates[1].URL {
		t.Fatalf("best stream = %q, want the 1080p variant", best[0].URL)
	}
	worst, err := streamsFromCandidates(candidates, testEpisode(), "sub", "https://mkissa.to", "worst")
	if err != nil {
		t.Fatalf("streamsFromCandidates() error = %v", err)
	}
	if worst[0].URL != candidates[0].URL {
		t.Fatalf("worst stream = %q, want the 480p variant", worst[0].URL)
	}
}

func TestStreamsFromCandidatesAttachesSubtitles(t *testing.T) {
	candidates := []browser.Candidate{
		playedCandidate(),
		{URL: "https://cdn.test/subs/ReHMC7TQnch3C6z8j/sub/1/en.vtt", Kind: "Fetch"},
	}
	streams, err := streamsFromCandidates(candidates, testEpisode(), "sub", "https://mkissa.to", "best")
	if err != nil {
		t.Fatalf("streamsFromCandidates() error = %v", err)
	}
	if len(streams) != 1 {
		t.Fatalf("streams = %d, want the subtitle track kept out of the playable list", len(streams))
	}
	if streams[0].Subtitle != candidates[1].URL {
		t.Fatalf("subtitle = %q, want %q", streams[0].Subtitle, candidates[1].URL)
	}
}

func TestStreamsFromCandidatesRejectsEmptyCaptures(t *testing.T) {
	_, err := streamsFromCandidates(nil, testEpisode(), "sub", "https://mkissa.to", "best")
	if err == nil {
		t.Fatal("streamsFromCandidates() error = nil, want a failure for an empty capture")
	}
}

func TestPlaybackHeadersCarryOnlyWhatTheBrowserSent(t *testing.T) {
	got := playbackHeaders(playedCandidate(), "https://mkissa.to")
	want := map[string]string{
		"Referer":    "https://player.test/player.html?id=abc",
		"User-Agent": browser.UserAgent,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("playbackHeaders() = %#v, want %#v", got, want)
	}
}

func TestPlaybackHeadersNeverLeakForeignCredentials(t *testing.T) {
	candidate := playedCandidate()
	candidate.Headers["Authorization"] = "Bearer anilist-token"
	candidate.Headers["X-Csrf-Token"] = "secret"
	got := playbackHeaders(candidate, "https://mkissa.to")
	for key := range got {
		if key != "Referer" && key != "User-Agent" && key != "Origin" && key != "Cookie" {
			t.Fatalf("playbackHeaders() forwarded %q, want only playback context", key)
		}
	}
}

func TestPlaybackHeadersFallBackToTheSiteOrigin(t *testing.T) {
	candidate := browser.Candidate{URL: "https://cdn.test/a.mp4", Kind: "Media"}
	got := playbackHeaders(candidate, "https://mkissa.to/")
	if got["Referer"] != "https://mkissa.to/" {
		t.Fatalf("Referer = %q, want the site origin", got["Referer"])
	}
	if got["User-Agent"] != browser.UserAgent {
		t.Fatalf("User-Agent = %q, want the browser user agent", got["User-Agent"])
	}
}

func TestStreamsUsesBrowserResolution(t *testing.T) {
	var pageURL atomic.Value
	client := NewClientWithEndpoints(nil, "https://api.test", "https://mkissa.to")
	client.UseBrowser(captureFunc(func(_ context.Context, req browser.Request) ([]browser.Candidate, error) {
		pageURL.Store(req.PageURL)
		return []browser.Candidate{playedCandidate()}, nil
	}))

	streams, err := client.Streams(t.Context(), testEpisode(), "sub", "best")
	if err != nil {
		t.Fatalf("Streams() error = %v", err)
	}
	if len(streams) != 1 || streams[0].URL != playedCandidate().URL {
		t.Fatalf("Streams() = %#v, want the captured media", streams)
	}
	if got := pageURL.Load(); got != "https://mkissa.to/anime/ReHMC7TQnch3C6z8j/p-1-sub" {
		t.Fatalf("page opened = %v, want the episode watch page", got)
	}
}

func TestStreamsPropagateCancellation(t *testing.T) {
	client := NewClientWithEndpoints(nil, "https://api.test", "https://mkissa.to")
	ctx, cancel := context.WithCancel(t.Context())
	client.UseBrowser(captureFunc(func(callCtx context.Context, _ browser.Request) ([]browser.Candidate, error) {
		cancel()
		return nil, callCtx.Err()
	}))

	_, err := client.Streams(ctx, testEpisode(), "sub", "best")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Streams() error = %v, want context.Canceled", err)
	}
}

func TestFailedResolutionIsNotCached(t *testing.T) {
	var captures atomic.Int32
	client := NewClientWithEndpoints(nil, "https://api.test", "https://mkissa.to")
	client.UseBrowser(captureFunc(func(context.Context, browser.Request) ([]browser.Candidate, error) {
		if captures.Add(1) == 1 {
			return nil, browser.ErrNoMedia
		}
		return []browser.Candidate{playedCandidate()}, nil
	}))

	if _, err := client.Streams(t.Context(), testEpisode(), "sub", "best"); err == nil {
		t.Fatal("Streams() error = nil, want the failed handoff to surface")
	}
	streams, err := client.Streams(t.Context(), testEpisode(), "sub", "best")
	if err != nil {
		t.Fatalf("Streams() after a failed handoff error = %v", err)
	}
	if len(streams) == 0 || streams[0].URL != playedCandidate().URL {
		t.Fatalf("Streams() = %#v, want the retry to resolve", streams)
	}
}

func TestStreamsCachesByEpisodeTranslationAndQuality(t *testing.T) {
	var captures atomic.Int32
	client := NewClientWithEndpoints(nil, "https://api.test", "https://mkissa.to")
	client.UseBrowser(captureFunc(func(context.Context, browser.Request) ([]browser.Candidate, error) {
		captures.Add(1)
		return []browser.Candidate{playedCandidate()}, nil
	}))

	for range 3 {
		if _, err := client.Streams(t.Context(), testEpisode(), "sub", "best"); err != nil {
			t.Fatalf("Streams() error = %v", err)
		}
	}
	if got := captures.Load(); got != 1 {
		t.Fatalf("browser captures = %d, want the repeat requests served from cache", got)
	}

	// A different quality, translation or episode is a different answer and
	// must not reuse the cached one.
	for _, variant := range []struct {
		episode     Episode
		translation string
		quality     string
	}{
		{testEpisode(), "sub", "worst"},
		{testEpisode(), "dub", "best"},
		{Episode{ShowID: "ReHMC7TQnch3C6z8j", Number: 2, Value: "2"}, "sub", "best"},
	} {
		if _, err := client.Streams(t.Context(), variant.episode, variant.translation, variant.quality); err != nil {
			t.Fatalf("Streams() error = %v", err)
		}
	}
	if got := captures.Load(); got != 4 {
		t.Fatalf("browser captures = %d, want one per distinct episode, translation and quality", got)
	}
}

func TestStreamCacheExpires(t *testing.T) {
	var captures atomic.Int32
	now := time.Unix(1_700_000_000, 0)
	client := NewClientWithEndpoints(nil, "https://api.test", "https://mkissa.to")
	client.now = func() time.Time { return now }
	client.UseBrowser(captureFunc(func(context.Context, browser.Request) ([]browser.Candidate, error) {
		captures.Add(1)
		return []browser.Candidate{playedCandidate()}, nil
	}))

	if _, err := client.Streams(t.Context(), testEpisode(), "sub", "best"); err != nil {
		t.Fatalf("Streams() error = %v", err)
	}
	now = now.Add(streamTTL + time.Second)
	if _, err := client.Streams(t.Context(), testEpisode(), "sub", "best"); err != nil {
		t.Fatalf("Streams() error = %v", err)
	}
	if got := captures.Load(); got != 2 {
		t.Fatalf("browser captures = %d, want the expired entry to be resolved again", got)
	}
}

func TestInvalidateStreamsForcesReresolution(t *testing.T) {
	var captures atomic.Int32
	client := NewClientWithEndpoints(nil, "https://api.test", "https://mkissa.to")
	client.UseBrowser(captureFunc(func(context.Context, browser.Request) ([]browser.Candidate, error) {
		captures.Add(1)
		return []browser.Candidate{playedCandidate()}, nil
	}))

	if _, err := client.Streams(t.Context(), testEpisode(), "sub", "best"); err != nil {
		t.Fatalf("Streams() error = %v", err)
	}
	client.InvalidateStreams(testEpisode(), "sub", "best")
	if _, err := client.Streams(t.Context(), testEpisode(), "sub", "best"); err != nil {
		t.Fatalf("Streams() error = %v", err)
	}
	if got := captures.Load(); got != 2 {
		t.Fatalf("browser captures = %d, want the invalidated entry to be resolved again", got)
	}
}

func TestCachedStreamsAreIsolatedFromCallers(t *testing.T) {
	client := NewClientWithEndpoints(nil, "https://api.test", "https://mkissa.to")
	client.UseBrowser(captureFunc(func(context.Context, browser.Request) ([]browser.Candidate, error) {
		return []browser.Candidate{playedCandidate()}, nil
	}))

	first, err := client.Streams(t.Context(), testEpisode(), "sub", "best")
	if err != nil {
		t.Fatalf("Streams() error = %v", err)
	}
	first[0].URL = "https://tampered.test/a.mp4"
	first[0].Headers["Referer"] = "https://tampered.test/"

	second, err := client.Streams(t.Context(), testEpisode(), "sub", "best")
	if err != nil {
		t.Fatalf("Streams() error = %v", err)
	}
	if second[0].URL != playedCandidate().URL {
		t.Fatalf("cached URL = %q, want the entry to survive caller mutation", second[0].URL)
	}
	if second[0].Headers["Referer"] == "https://tampered.test/" {
		t.Fatal("cached headers were mutated through a returned stream")
	}
}

func TestResolutionHint(t *testing.T) {
	cases := map[string]int{
		"https://cdn.test/1080p/index.m3u8":     1080,
		"https://cdn.test/video-720p.mp4":       720,
		"https://cdn.test/hls/master.m3u8":      0,
		"https://cdn.test/9999p/index.m3u8":     0,
		"https://cdn.test/a/480p/b/1080p/i.m3u": 1080,
	}
	for raw, want := range cases {
		if got := resolutionHint(raw); got != want {
			t.Fatalf("resolutionHint(%q) = %d, want %d", raw, got, want)
		}
	}
}
