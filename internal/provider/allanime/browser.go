package allanime

import (
	"context"
	"fmt"
	"net/url"
	"path/filepath"
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

// browserSettle is how long the capture keeps observing after the first match,
// so a subtitle track or a second quality variant requested moments later is
// still picked up.
const browserSettle = 2 * time.Second

// UseBrowser makes the client resolve streams from a browser session first and
// fall back to the native crypto path when the session yields nothing.
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
	mediaPlaylist
	mediaProgressive
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

// classify names what a captured request is. showID, episodeValue and
// translation describe the episode that was asked for; passing them empty
// re-reads the shape of an already accepted candidate without re-checking
// which episode it belongs to.
func classify(candidate browser.Candidate, showID, episodeValue, translation string) mediaClass {
	parsed, kind := shapeOf(candidate)
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
	switch {
	case hasAnySuffix(path, segmentSuffixes):
		// A chunk plays for a few seconds on its own and is never the answer
		// to "where is this episode".
		return parsed, mediaNone
	case hasAnySuffix(path, subtitleSuffixes):
		return parsed, mediaSubtitle
	case hasAnySuffix(path, playlistSuffixes):
		return parsed, mediaPlaylist
	case candidate.Kind == "Media":
		return parsed, mediaPlayed
	case hasAnySuffix(path, progressiveSuffixes):
		return parsed, mediaProgressive
	}
	return parsed, mediaNone
}

// correlates reports whether a URL names the episode that was requested. It is
// what keeps a stale player, a preview of the next episode, or an unrelated
// autoplay from being returned as the answer.
func correlates(parsed *url.URL, showID, episodeValue, translation string) bool {
	path := strings.ToLower(parsed.Path)
	if showID != "" && !strings.Contains(path, strings.ToLower(showID)) {
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
		if segment == wanted || strings.TrimSuffix(segment, filepath.Ext(segment)) == wanted {
			return true
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
	for _, candidate := range candidates {
		parsed, class := shapeOf(candidate)
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
		return nil, fmt.Errorf("browser session captured no playable media")
	}

	worst := strings.EqualFold(quality, "worst")
	sort.SliceStable(playable, func(i, j int) bool {
		// A URL that names the requested episode always beats one that only
		// happens to have been loaded while the page was open.
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

// playbackHeaders reduces the request Chromium made to the smallest set an
// external player needs. Only headers the browser itself sent to that host are
// forwarded, so no cookie or credential from an unrelated origin can leak into
// the player command line.
func playbackHeaders(candidate browser.Candidate, origin string) map[string]string {
	wanted := map[string]string{"referer": "Referer", "user-agent": "User-Agent", "origin": "Origin", "cookie": "Cookie"}
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
