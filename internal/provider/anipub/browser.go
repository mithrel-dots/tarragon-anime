package anipub

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"tarragon-anime/internal/browser"
	"tarragon-anime/internal/streamlog"
)

// Resolver captures the requests a real browser session makes while the site's
// own player loads an episode.
type Resolver interface {
	Capture(context.Context, browser.Request) ([]browser.Candidate, error)
}

// browserSettle is how long the capture keeps observing after the first
// playlist, so the subtitle track the player fetches alongside it is not
// missed.
const browserSettle = 2 * time.Second

// UseBrowser configures the browser session used to resolve streams.
func (c *Client) UseBrowser(resolver Resolver) { c.browser = resolver }

func (c *Client) browserStreams(ctx context.Context, episode Episode, translation, quality string) ([]Stream, error) {
	page := watchURL(c.base, episode.ShowID, episode.Value, translation)
	candidates, err := c.browser.Capture(ctx, browser.Request{
		PageURL: page,
		Settle:  browserSettle,
		Accept:  func(candidate browser.Candidate) bool { return classify(candidate) != mediaNone },
		// Only a playlist starts the settle window. Anchoring on a subtitle
		// would end the capture before the media it belongs to appears.
		Ready: func(candidate browser.Candidate) bool { return classify(candidate) == mediaPlaylist },
	})
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Selection happens in prepareStream after the captured master is fetched.
	_ = quality
	source := "default"
	if len(candidates) > 0 {
		source = browser.SourceID(candidates[0].Source)
	}
	finish := streamlog.Start(ctx, source, "validation")
	streams, err := streamsFromCandidates(candidates)
	switch {
	case errors.Is(err, errCredentials):
		finish("unavailable", "credentials_required")
	case err != nil:
		finish("unavailable", "no_playable_media")
	default:
		finish("resolved", "validated_media")
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return streams, err
}

var errCredentials = errors.New("browser media requires Cookie or Authorization headers; use browser playback until an origin-scoped credential handoff is available")

type mediaClass int

const (
	mediaNone mediaClass = iota
	mediaSubtitle
	mediaPlaylist
)

// adHosts serve their own media beside the player and must be rejected before
// any other rule.
var adHosts = []string{
	"doubleclick.net",
	"googlesyndication.com",
	"google-analytics.com",
	"googletagmanager.com",
	"imasdk.googleapis.com",
	"2mdn.net",
	"adservice.google.com",
	"scorecardresearch.com",
	"moatads.com",
	"tiktokcdn.com",
}

var subtitleSuffixes = []string{".vtt", ".srt", ".ass", ".ssa"}

// classify decides what an observed request is. The site plays HLS, so the
// only thing worth handing to a player is a playlist; individual segments are
// rejected because a chunk is never the answer to "where is this episode".
func classify(candidate browser.Candidate) mediaClass {
	if candidate.Status < 200 || candidate.Status >= 300 {
		return mediaNone
	}
	parsed, err := url.Parse(candidate.URL)
	// blob: and data: URLs exist only inside the browser and cannot be handed
	// to an external player.
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return mediaNone
	}
	if isAdHost(parsed.Host) {
		return mediaNone
	}
	path := strings.ToLower(parsed.Path)
	mime := strings.ToLower(strings.TrimSpace(strings.SplitN(candidate.MIME, ";", 2)[0]))
	switch {
	case hasAnySuffix(path, subtitleSuffixes):
		return mediaSubtitle
	case strings.HasSuffix(path, ".m3u8"),
		mime == "application/vnd.apple.mpegurl", mime == "application/x-mpegurl",
		mime == "audio/mpegurl", mime == "audio/x-mpegurl":
		return mediaPlaylist
	}
	return mediaNone
}

// streamsFromCandidates turns the captured requests into provider streams. A
// master playlist is preferred over a variant: it lists every rendition, so the
// player picks the quality rather than this code guessing one.
func streamsFromCandidates(candidates []browser.Candidate) ([]Stream, error) {
	type ranked struct {
		candidate browser.Candidate
		master    bool
	}
	var playlists []ranked
	subtitle := ""
	credentialsRejected := false
	for _, candidate := range candidates {
		credentials := false
		for key := range candidate.Headers {
			if strings.EqualFold(key, "Cookie") || strings.EqualFold(key, "Authorization") {
				credentials = true
			}
		}
		if credentials {
			credentialsRejected = credentialsRejected || classify(candidate) != mediaNone
			continue
		}
		switch classify(candidate) {
		case mediaSubtitle:
			if subtitle == "" {
				subtitle = candidate.URL
			}
		case mediaPlaylist:
			playlists = append(playlists, ranked{candidate: candidate, master: isMasterPlaylist(candidate.URL)})
		}
	}
	if len(playlists) == 0 {
		if credentialsRejected {
			return nil, errors.Join(browser.ErrNoMedia, errCredentials)
		}
		return nil, fmt.Errorf("%w: browser session captured no playlist", browser.ErrNoMedia)
	}

	sort.SliceStable(playlists, func(i, j int) bool {
		if playlists[i].master != playlists[j].master {
			return playlists[i].master
		}
		return playlists[i].candidate.Observed < playlists[j].candidate.Observed
	})

	streams := make([]Stream, 0, len(playlists))
	for _, item := range playlists {
		streams = append(streams, Stream{
			Source:   browser.SourceID(item.candidate.Source),
			URL:      item.candidate.URL,
			Headers:  playbackHeaders(item.candidate),
			Subtitle: subtitle,
		})
	}
	return streams, nil
}

// isMasterPlaylist recognises the multi-rendition playlist by name. The body is
// not available at capture time, so the filename is the only signal, and
// getting it wrong costs quality selection rather than playback.
func isMasterPlaylist(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(parsed.Path), "master")
}

// playbackHeaders forwards only the playback context the browser actually
// sent. A header invented on the player's behalf is not neutral: a CDN that
// signs its URLs validates the referrer against the one it issued the token
// for, and an unexpected value is rejected outright.
func playbackHeaders(candidate browser.Candidate) map[string]string {
	wanted := map[string]string{"referer": "Referer", "user-agent": "User-Agent", "origin": "Origin"}
	headers := make(map[string]string, len(wanted))
	for key, value := range candidate.Headers {
		if canonical, ok := wanted[strings.ToLower(key)]; ok && value != "" {
			headers[canonical] = value
		}
	}
	if headers["User-Agent"] == "" {
		headers["User-Agent"] = browser.UserAgent
	}
	return headers
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
