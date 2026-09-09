package allanime

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"tarragon-anime/internal/browser"
)

// Resolver captures the requests a real browser session makes while the
// provider's own player loads an episode.
type Resolver interface {
	Capture(context.Context, browser.Request) ([]browser.Candidate, error)
}

// browserSettle is how long the capture keeps observing after the first media match,
// so a subtitle track or a second quality variant requested moments later is
// still picked up.
const browserSettle = 2 * time.Second

// UseBrowser configures the browser session used to resolve streams.
func (c *Client) UseBrowser(resolver Resolver) {
	c.browser = resolver
}

// watchURL builds the site's episode page URL. The slug shape is part of the
// site's routing contract rather than of its JavaScript, so it survives bundle
// changes.
func watchURL(origin, showID, episodeValue, translation string) string {
	return fmt.Sprintf("%s/anime/%s/p-%s-%s",
		strings.TrimRight(origin, "/"),
		url.PathEscape(showID),
		url.PathEscape(episodeValue),
		url.PathEscape(strings.ToLower(translation)),
	)
}

func (c *Client) browserStreams(ctx context.Context, episode Episode, translation, quality string) ([]Stream, error) {
	candidates, err := c.browser.Capture(ctx, browser.Request{
		PageURL: watchURL(c.origin, episode.ShowID, episode.Value, translation),
		Settle:  browserSettle,
		Accept:  acceptMedia(episode.ShowID, episode.Value, translation),
		Ready: func(candidate browser.Candidate) bool {
			class := classify(candidate, episode.ShowID, episode.Value, translation)
			return class != mediaNone && class != mediaSubtitle
		},
	})
	if err != nil {
		return nil, err
	}
	return streamsFromCandidates(candidates, episode, translation, c.origin, quality)
}

type mediaClass int

const (
	mediaNone mediaClass = iota
	mediaSubtitle
	mediaProgressive
	mediaPlaylist
	// mediaPlayed is the request the player actually fed to the video
	// element, which is the strongest evidence that a URL is the episode.
	mediaPlayed
)

// adHosts are the third party embeds the page loads alongside the player. They
// serve their own media, so they are rejected before any other rule.
var adHosts = []string{
	"doubleclick.net",
	"googlesyndication.com",
	"google-analytics.com",
	"googletagmanager.com",
	"imasdk.googleapis.com",
	"2mdn.net",
	"adservice.google.com",
	"mc.yandex.ru",
	"mc.yandex.com",
	"mail.ru",
	"scorecardresearch.com",
	"moatads.com",
}

// segmentSuffixes identify individual HLS or DASH chunks. A chunk plays for a
// few seconds on its own and is never the answer to "where is this episode".
var segmentSuffixes = []string{".ts", ".m4s", ".aac", ".key", ".init", ".cmfv", ".cmfa"}

var playlistSuffixes = []string{".m3u8", ".mpd"}

var subtitleSuffixes = []string{".vtt", ".srt", ".ass", ".ssa"}

var progressiveSuffixes = []string{".mp4", ".mkv", ".webm", ".m4v"}

// acceptMedia builds the predicate handed to the browser. It is deliberately
// permissive about which of the episode's own URLs it lets through and strict
// about everything else; ranking happens later, on the full candidate set.
func acceptMedia(showID, episodeValue, translation string) func(browser.Candidate) bool {
	return func(candidate browser.Candidate) bool {
		return classify(candidate, showID, episodeValue, translation) != mediaNone
	}
}

// classify rejects explicit identity mismatches even for played media and
// manifests. Opaque URLs remain eligible: capture timing and request kind are
// evidence, not proof that they belong to the requested episode.
func classify(candidate browser.Candidate, showID, episodeValue, translation string) mediaClass {
	parsed, kind := shapeOf(candidate)
	if kind == mediaNone || mismatches(parsed, showID, episodeValue, translation) {
		return mediaNone
	}
	switch kind {
	case mediaNone, mediaSubtitle, mediaPlaylist, mediaPlayed:
		return kind
	}
	// A progressive URL that the player never loaded is only the episode when
	// the URL itself says so; otherwise it is a trailer, a preview, or the
	// next episode being prefetched by the page.
	if correlates(parsed, showID, episodeValue, translation) {
		return kind
	}
	return mediaNone
}

func shapeOf(candidate browser.Candidate) (*url.URL, mediaClass) {
	if candidate.Status < 200 || candidate.Status >= 300 {
		return nil, mediaNone
	}
	parsed, err := url.Parse(candidate.URL)
	// blob: and data: URLs only exist inside the browser and cannot be handed
	// to an external player, so they are dropped here.
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, mediaNone
	}
	if isAdHost(parsed.Host) {
		return parsed, mediaNone
	}
	path := strings.ToLower(parsed.Path)
	mime := strings.ToLower(strings.TrimSpace(strings.SplitN(candidate.MIME, ";", 2)[0]))
	switch {
	case hasAnySuffix(path, segmentSuffixes):
		// A chunk plays for a few seconds on its own and is never the answer
		// to "where is this episode".
		return parsed, mediaNone
	case hasAnySuffix(path, subtitleSuffixes):
		return parsed, mediaSubtitle
	case hasAnySuffix(path, playlistSuffixes), mime == "application/vnd.apple.mpegurl",
		mime == "application/x-mpegurl", mime == "audio/mpegurl", mime == "audio/x-mpegurl",
		mime == "application/dash+xml":
		return parsed, mediaPlaylist
	case candidate.Kind == "Media":
		return parsed, mediaPlayed
	case hasAnySuffix(path, progressiveSuffixes):
		return parsed, mediaProgressive
	}
	return parsed, mediaNone
}

// Only the known show/translation/episode path layout gives us an explicit
// identity. Arbitrary CDN paths and query tokens cannot be reliably decoded.
func mismatches(parsed *url.URL, showID, episodeValue, translation string) bool {
	segments := strings.Split(strings.ToLower(parsed.Path), "/")
	for i, segment := range segments {
		if (segment != "sub" && segment != "dub") || i < 2 || i+1 >= len(segments) || segments[i+1] == "" {
			continue
		}
		episode := segments[i+1]
		for _, suffixes := range [][]string{playlistSuffixes, progressiveSuffixes, subtitleSuffixes} {
			for _, suffix := range suffixes {
				episode = strings.TrimSuffix(episode, suffix)
			}
		}
		if (showID != "" && segments[i-1] != strings.ToLower(showID)) ||
			(translation != "" && segment != strings.ToLower(translation)) ||
			(episodeValue != "" && episode != strings.ToLower(episodeValue)) {
			return true
		}
	}
	return false
}

// correlates reports whether a URL names the episode that was requested. It is
// what keeps a stale player, a preview of the next episode, or an unrelated
// autoplay from being returned as the answer.
func correlates(parsed *url.URL, showID, episodeValue, translation string) bool {
	path := strings.ToLower(parsed.Path)
	if showID != "" && !strings.Contains(path, "/"+strings.ToLower(showID)+"/") {
		return false
	}
	if translation != "" && !strings.Contains(path, "/"+strings.ToLower(translation)+"/") {
		return false
	}
	if episodeValue == "" {
		return true
	}
	wanted := strings.ToLower(episodeValue)
	for _, segment := range strings.Split(path, "/") {
		// Upstreams name the episode either as a bare path segment or as a
		// file, so "1", "1.mp4" and "1.m3u8" all identify episode 1.
		if segment == wanted {
			return true
		}
		for _, suffixes := range [][]string{progressiveSuffixes, playlistSuffixes} {
			for _, suffix := range suffixes {
				if segment == wanted+suffix {
					return true
				}
			}
		}
	}
	return false
}

func isAdHost(host string) bool {
	host = strings.ToLower(host)
	if index := strings.LastIndex(host, ":"); index > 0 {
		host = host[:index]
	}
	for _, blocked := range adHosts {
		if host == blocked || strings.HasSuffix(host, "."+blocked) {
			return true
		}
	}
	return false
}

func hasAnySuffix(value string, suffixes []string) bool {
	for _, suffix := range suffixes {
		if strings.HasSuffix(value, suffix) {
			return true
		}
	}
	return false
}

// streamsFromCandidates ranks the captured requests and converts the playable
// ones into provider streams, attaching any subtitle track that was captured
// alongside them.
func streamsFromCandidates(candidates []browser.Candidate, episode Episode, translation, origin, quality string) ([]Stream, error) {
	type ranked struct {
		candidate  browser.Candidate
		class      mediaClass
		correlated bool
		height     int
	}
	var playable []ranked
	var subtitle string
	credentialsRejected := false
	for _, candidate := range candidates {
		class := classify(candidate, episode.ShowID, episode.Value, translation)
		if class == mediaNone {
			continue
		}
		if hasCredentials(candidate) {
			credentialsRejected = true
			continue
		}
		parsed, _ := url.Parse(candidate.URL)
		switch class {
		case mediaNone:
			continue
		case mediaSubtitle:
			if subtitle == "" {
				subtitle = candidate.URL
			}
			continue
		}
		playable = append(playable, ranked{
			candidate:  candidate,
			class:      class,
			correlated: correlates(parsed, episode.ShowID, episode.Value, translation),
			height:     resolutionHint(candidate.URL),
		})
	}
	if len(playable) == 0 {
		if credentialsRejected {
			return nil, fmt.Errorf("browser media requires Cookie or Authorization headers; use browser playback until an origin-scoped credential handoff is available")
		}
		return nil, fmt.Errorf("browser session captured no playable media")
	}

	worst := strings.EqualFold(quality, "worst")
	sort.SliceStable(playable, func(i, j int) bool {
		// Correlated Media is strongest. Otherwise manifests beat speculative
		// MP4 requests, which may be initialization or media fragments.
		iPlayed := playable[i].class == mediaPlayed && playable[i].correlated
		jPlayed := playable[j].class == mediaPlayed && playable[j].correlated
		if iPlayed != jPlayed {
			return iPlayed
		}
		iManifest := playable[i].class == mediaPlaylist
		jManifest := playable[j].class == mediaPlaylist
		if iManifest != jManifest {
			return iManifest
		}
		if playable[i].correlated != playable[j].correlated {
			return playable[i].correlated
		}
		if playable[i].class != playable[j].class {
			return playable[i].class > playable[j].class
		}
		if playable[i].height != playable[j].height {
			if worst {
				return playable[i].height < playable[j].height
			}
			return playable[i].height > playable[j].height
		}
		return playable[i].candidate.Observed < playable[j].candidate.Observed
	})

	streams := make([]Stream, 0, len(playable))
	for _, item := range playable {
		streams = append(streams, Stream{
			URL:      item.candidate.URL,
			Headers:  playbackHeaders(item.candidate, origin),
			Subtitle: subtitle,
		})
	}
	return streams, nil
}

func hasCredentials(candidate browser.Candidate) bool {
	for key := range candidate.Headers {
		if strings.EqualFold(key, "Cookie") || strings.EqualFold(key, "Authorization") {
			return true
		}
	}
	return false
}

// playbackHeaders forwards only non-credential playback context. Global player
// headers are not origin-scoped and may reach redirects or manifest children.
func playbackHeaders(candidate browser.Candidate, origin string) map[string]string {
	wanted := map[string]string{"referer": "Referer", "user-agent": "User-Agent", "origin": "Origin"}
	headers := make(map[string]string, len(wanted))
	for key, value := range candidate.Headers {
		if canonical, ok := wanted[strings.ToLower(key)]; ok && value != "" {
			headers[canonical] = value
		}
	}
	if headers["Referer"] == "" && origin != "" {
		headers["Referer"] = strings.TrimRight(origin, "/") + "/"
	}
	if headers["User-Agent"] == "" {
		headers["User-Agent"] = browser.UserAgent
	}
	return headers
}

// resolutionHint reads a vertical resolution out of a URL when the upstream
// encodes one, which is how quality preference is honoured for playlist and
// per-quality progressive sources.
func resolutionHint(raw string) int {
	best := 0
	for _, field := range strings.FieldsFunc(strings.ToLower(raw), func(r rune) bool {
		return r < '0' || (r > '9' && r < 'a') || r > 'z'
	}) {
		if !strings.HasSuffix(field, "p") {
			continue
		}
		value, err := strconv.Atoi(strings.TrimSuffix(field, "p"))
		if err != nil || value < 144 || value > 4320 {
			continue
		}
		if value > best {
			best = value
		}
	}
	return best
}
