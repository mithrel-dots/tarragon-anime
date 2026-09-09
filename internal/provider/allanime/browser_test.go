package allanime

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
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
		// Observed upstream: the episode file carries a cache-busting suffix
		// rather than being named after the episode alone.
		{"episode with cache busting suffix", browser.Candidate{
			URL:  "https://tools.fast4speed.rsvp/media9/videos/ReHMC7TQnch3C6z8j/sub/1_1788962900147-dbdw4nn0?Authorization=token",
			Kind: "Media", MIME: "application/octet-stream",
		}, true},
		// Observed upstream: the player fetches animated emoji through a media
		// element, so Chromium reports them exactly like the episode.
		{"animated emoji played as media", browser.Candidate{
			URL:  "https://aln.youtube-anime.com/mcovers/emojis/1490419827691749478.gif",
			Kind: "Media", MIME: "image/gif",
		}, false},
		{"cover art played as media", browser.Candidate{
			URL: "https://aln.youtube-anime.com/mcovers/art.webp", Kind: "Media", MIME: "image/webp",
		}, false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			test.candidate.Status = 200
			if got := accept(test.candidate); got != test.want {
				t.Fatalf("acceptMedia()(%q) = %t, want %t", test.candidate.URL, got, test.want)
			}
		})
	}
}

// TestStreamsFromCandidatesPrefersTheEpisodeOverPlayedDecoys reproduces the
// capture that handed mpv a three second emoji loop: the page plays both the
// episode and an animated emoji through a media element, and the episode file
// is named with a cache-busting suffix.
func TestStreamsFromCandidatesPrefersTheEpisodeOverPlayedDecoys(t *testing.T) {
	episode := Episode{ShowID: "SyR2K6bGYfKSE6YMm", Number: 16, Value: "16"}
	want := "https://tools.fast4speed.rsvp/media9/videos/SyR2K6bGYfKSE6YMm/sub/16_1788962900147-dbdw4nn0?Authorization=token"
	candidates := []browser.Candidate{
		{URL: want, Kind: "Media", MIME: "application/octet-stream", Status: 206, Observed: 2 * time.Second},
		{
			URL:  "https://aln.youtube-anime.com/mcovers/emojis/1490419827691749478.gif",
			Kind: "Media", MIME: "image/gif", Status: 206, Observed: time.Second,
		},
	}
	streams, err := streamsFromCandidates(candidates, episode, "sub", "https://mkissa.to", "best")
	if err != nil {
		t.Fatalf("streamsFromCandidates() error = %v", err)
	}
	if len(streams) != 1 || streams[0].URL != want {
		t.Fatalf("streams = %#v, want only the episode media", streams)
	}
}

func TestNamesEpisodeRequiresASeparator(t *testing.T) {
	cases := []struct {
		segment, episode string
		want             bool
	}{
		{"16", "16", true},
		{"16.mp4", "16", true},
		{"16_1788962900147-dbdw4nn0", "16", true},
		{"16-1080p.m3u8", "16", true},
		{"160", "16", false},
		{"160_1788962900147", "16", false},
		{"1", "16", false},
		{"", "16", false},
		{"16", "", false},
	}
	for _, test := range cases {
		if got := namesEpisode(test.segment, test.episode); got != test.want {
			t.Fatalf("namesEpisode(%q, %q) = %t, want %t", test.segment, test.episode, got, test.want)
		}
	}
}

func TestStreamsFromCandidatesPrefersThePlayedEpisode(t *testing.T) {
	candidates := []browser.Candidate{
		{URL: "https://cdn.test/videos/otherShowId/sub/1.mp4", Kind: "XHR", Status: 200, Observed: time.Millisecond},
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
		{URL: "https://cdn.test/videos/ReHMC7TQnch3C6z8j/sub/1/480p.m3u8", Kind: "XHR", Status: 200},
		{URL: "https://cdn.test/videos/ReHMC7TQnch3C6z8j/sub/1/1080p.m3u8", Kind: "XHR", Status: 200},
		{URL: "https://cdn.test/videos/ReHMC7TQnch3C6z8j/sub/1/720p.m3u8", Kind: "XHR", Status: 200},
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
		{URL: "https://cdn.test/subs/ReHMC7TQnch3C6z8j/sub/1/en.vtt", Kind: "Fetch", Status: 200},
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
	candidate.Headers["cOoKiE"] = "session=secret"
	candidate.Headers["X-Csrf-Token"] = "secret"
	got := playbackHeaders(candidate, "https://mkissa.to")
	for key := range got {
		if key != "Referer" && key != "User-Agent" && key != "Origin" {
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
		if req.Ready == nil {
			t.Fatal("browser request has no readiness predicate")
		}
		for _, candidate := range []browser.Candidate{
			playedCandidate(),
			{URL: "https://cdn.test/master.m3u8", Status: 200},
			{URL: "https://cdn.test/en.vtt", Status: 200},
			{URL: "https://cdn.test/cover.png", Status: 200},
			{URL: "https://cdn.test/otherShow/sub/1.mp4", Kind: "Media", Status: 200},
		} {
			want := req.Accept(candidate) && !strings.HasSuffix(candidate.URL, ".vtt")
			if req.Ready(candidate) != want {
				t.Errorf("Ready(%q) = %t, want %t", candidate.URL, req.Ready(candidate), want)
			}
		}
		if !req.Accept(browser.Candidate{URL: "https://cdn.test/en.vtt", Status: 200}) {
			t.Fatal("readiness filtering excluded accepted subtitles")
		}
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
	client.UseBrowser(captureFunc(func(_ context.Context, req browser.Request) ([]browser.Candidate, error) {
		captures.Add(1)
		identity := strings.Split(strings.TrimPrefix(req.PageURL, "https://mkissa.to/anime/"), "/p-")
		variant := strings.Split(identity[1], "-")
		candidate := playedCandidate()
		candidate.URL = fmt.Sprintf("https://cdn.test/videos/%s/%s/%s", identity[0], variant[1], variant[0])
		return []browser.Candidate{candidate}, nil
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

func TestCandidateIdentityIsRechecked(t *testing.T) {
	for _, path := range []string{
		"otherShowId/sub/1", "ReHMC7TQnch3C6z8j/sub/7", "ReHMC7TQnch3C6z8j/dub/1",
		"ReHMC7TQnch3C6z8j-extra/sub/1",
	} {
		for _, suffix := range []string{"", ".mp4", ".m3u8", ".mpd", "/en.vtt"} {
			for _, kind := range []string{"Media", "XHR"} {
				t.Run(path+suffix+kind, func(t *testing.T) {
					candidate := browser.Candidate{URL: "https://cdn.test/videos/" + path + suffix, Kind: kind, Status: 200}
					if acceptMedia(testEpisode().ShowID, "1", "sub")(candidate) {
						t.Fatal("accepted explicit identity mismatch")
					}
					streams, err := streamsFromCandidates([]browser.Candidate{candidate, playedCandidate()}, testEpisode(), "sub", "", "best")
					if err != nil || len(streams) != 1 || streams[0].URL != playedCandidate().URL || streams[0].Subtitle != "" {
						t.Fatalf("mismatched candidate survived conversion: %#v, %v", streams, err)
					}
				})
			}
		}
	}
}

func TestManifestMIMEAndRanking(t *testing.T) {
	for _, mime := range []string{"application/vnd.apple.mpegurl", "Application/X-MpegURL; charset=utf-8", "audio/mpegurl", "audio/x-mpegurl", "application/dash+xml"} {
		t.Run(mime, func(t *testing.T) {
			manifest := browser.Candidate{URL: "https://cdn.test/opaque/manifest?token=abc", MIME: mime, Kind: "XHR", Status: 200}
			if !acceptMedia(testEpisode().ShowID, "1", "sub")(manifest) {
				t.Fatal("extensionless manifest rejected")
			}
			candidates := []browser.Candidate{
				{URL: "https://cdn.test/videos/ReHMC7TQnch3C6z8j/sub/1/init.mp4", Kind: "XHR", Status: 200},
				{URL: "https://cdn.test/videos/ReHMC7TQnch3C6z8j/sub/1/segment-1.mp4", Kind: "XHR", Status: 206},
				manifest,
			}
			for _, quality := range []string{"best", "worst"} {
				streams, err := streamsFromCandidates(candidates, testEpisode(), "sub", "", quality)
				if err != nil || streams[0].URL != manifest.URL {
					t.Fatalf("manifest did not beat speculative MP4: %#v, %v", streams, err)
				}
				streams, err = streamsFromCandidates(append(candidates, playedCandidate()), testEpisode(), "sub", "", quality)
				if err != nil || streams[0].URL != playedCandidate().URL {
					t.Fatalf("manifest displaced correlated played media: %#v, %v", streams, err)
				}
			}
		})
	}
	for _, candidate := range []browser.Candidate{
		{URL: "https://cdn.test/opaque", Kind: "Media", Status: 206},
		{URL: "https://cdn.test/opaque/master.m3u8", Kind: "XHR", Status: 200},
	} {
		streams, err := streamsFromCandidates([]browser.Candidate{candidate}, testEpisode(), "sub", "", "best")
		if err != nil || len(streams) != 1 {
			t.Fatalf("opaque candidate rejected: %#v, %v", streams, err)
		}
	}
}

func TestStreamsRejectNonSuccessStatuses(t *testing.T) {
	for _, status := range []int{0, 101, 199, 300, 302, 403, 404, 500} {
		for _, suffix := range []string{".mp4", ".m3u8", ".vtt"} {
			candidate := browser.Candidate{URL: "https://cdn.test/opaque" + suffix, Kind: "Media", Status: status}
			if acceptMedia(testEpisode().ShowID, "1", "sub")(candidate) {
				t.Fatalf("accepted status %d", status)
			}
			streams, err := streamsFromCandidates([]browser.Candidate{candidate, playedCandidate()}, testEpisode(), "sub", "", "best")
			if err != nil || len(streams) != 1 || streams[0].Subtitle != "" {
				t.Fatalf("status %d survived conversion: %#v, %v", status, streams, err)
			}
			if _, err := streamsFromCandidates([]browser.Candidate{candidate}, testEpisode(), "sub", "", "best"); err == nil {
				t.Fatalf("status %d returned without error", status)
			}
		}
	}
}

func TestStreamsRejectCredentialHandoff(t *testing.T) {
	for _, header := range []string{"Cookie", "cOoKiE", "Authorization", "aUtHoRiZaTiOn"} {
		t.Run(header, func(t *testing.T) {
			candidate := playedCandidate()
			candidate.Headers[header] = "secret-value"
			streams, err := streamsFromCandidates([]browser.Candidate{candidate}, testEpisode(), "sub", "", "best")
			if len(streams) != 0 || err == nil || !strings.Contains(err.Error(), "origin-scoped") || !strings.Contains(err.Error(), "use browser playback") || strings.Contains(err.Error(), "secret-value") {
				t.Fatalf("want actionable, secret-free credential error, got %#v, %v", streams, err)
			}
			subtitle := candidate
			subtitle.URL = "https://cdn.test/subs/en.vtt"
			streams, err = streamsFromCandidates([]browser.Candidate{candidate, subtitle, playedCandidate()}, testEpisode(), "sub", "", "best")
			if err != nil || len(streams) != 1 || streams[0].Subtitle != "" {
				t.Fatalf("credential media or subtitle survived: %#v, %v", streams, err)
			}
			for key := range playbackHeaders(candidate, "") {
				if strings.EqualFold(key, "Cookie") || strings.EqualFold(key, "Authorization") {
					t.Fatalf("credential header forwarded: %s", key)
				}
			}
		})
	}
}
