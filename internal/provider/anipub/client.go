// Package anipub resolves AniPub metadata and MegaPlay streams over HTTP, with
// an optional browser fallback when the native player adapter becomes outdated.
package anipub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"tarragon-anime/internal/browser"
	"tarragon-anime/internal/provider"
	"tarragon-anime/internal/streamlog"
)

// Anime, Episode and Stream are the provider-neutral values exchanged with the
// service; the package keeps no separate public model.
type (
	Anime   = provider.Anime
	Episode = provider.Episode
	Stream  = provider.Stream
)

const (
	defaultBaseURL = "https://anipub.xyz"
	// userAgent is sent on the metadata calls so they look like the site's own
	// fetches. Playback headers are taken from the browser session instead.
	userAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/153.0.0.0 Safari/537.36"
	// maxBody bounds a metadata response. The catalogue endpoints answer in
	// kilobytes; anything beyond this is a redirect to something unexpected.
	maxBody = 4 << 20
)

// playLinkRE pulls the play route out of a details entry. The site stores the
// value as an iframe attribute fragment rather than a bare URL, and the
// episode number is a path segment inside it.
var playLinkRE = regexp.MustCompile(`/play/(\d+)/(\d+)(?:/([a-zA-Z]+))?`)
var videoLinkRE = regexp.MustCompile(`/video/(\d+)(?:/([a-zA-Z]+))?`)

type Client struct {
	http   *http.Client
	base   string
	logger *log.Logger

	browser Resolver

	cacheMu      sync.Mutex
	streamCache  map[string]streamEntry
	episodeCache map[string]episodeEntry

	proxyMu sync.Mutex
	proxy   *mediaProxy
	closed  bool
}

var _ provider.Client = (*Client)(nil)
var _ provider.EpisodeLookup = (*Client)(nil)
var _ provider.MALEpisodeLookup = (*Client)(nil)

// NewClient returns a client using the public AniPub endpoints.
func NewClient(httpClient *http.Client) *Client {
	return NewClientWithBaseURL(httpClient, defaultBaseURL)
}

// NewClientWithBaseURL points the client at an alternate origin, which is what
// the tests use to serve the catalogue from a local server.
func NewClientWithBaseURL(httpClient *http.Client, baseURL string) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	return &Client{
		http:         httpClient,
		base:         strings.TrimRight(baseURL, "/"),
		logger:       log.New(io.Discard, "", 0),
		streamCache:  map[string]streamEntry{},
		episodeCache: map[string]episodeEntry{},
	}
}

// WithLogger attaches a logger for resolution diagnostics.
func (c *Client) WithLogger(logger *log.Logger) *Client {
	if logger != nil {
		c.logger = logger
	}
	return c
}

type searchResult struct {
	Name   string `json:"Name"`
	ID     int    `json:"Id"`
	Finder string `json:"finder"`
	Error  string `json:"error"`
}

type detailsResponse struct {
	Error string `json:"error"`
	Local struct {
		Name   string `json:"name"`
		Finder string `json:"finder"`
		// Link is the first episode. The site keeps it beside the list rather
		// than inside it, so it has to be read separately or episode one is
		// silently missing.
		Link     string `json:"link"`
		Episodes []struct {
			Link string `json:"link"`
		} `json:"ep"`
	} `json:"local"`
}

type episodeEntry struct {
	episodes     []Episode
	expires      time.Time
	directShowID string // Only set when every raw link confirms the same /play show.
}

// Match finds the show on AniPub. Only an exact title match is accepted: the
// catalogue carries seasons, specials and recap cuts under names that differ
// from the requested one by a word, and picking a near miss plays the wrong
// episode of the wrong show rather than failing visibly.
func (c *Client) Match(ctx context.Context, mediaID int, aliases []string, _ string) (Anime, error) {
	for _, query := range uniqueTitles(aliases) {
		results, err := c.search(ctx, query)
		if err != nil {
			return Anime{}, err
		}
		matches := map[int]searchResult{}
		for _, result := range results {
			if result.ID != 0 && normalizeTitle(result.Name) == normalizeTitle(query) {
				matches[result.ID] = result
			}
		}
		switch len(matches) {
		case 0:
			continue
		case 1:
			for _, match := range matches {
				return Anime{
					ID:        strconv.Itoa(match.ID),
					Name:      match.Name,
					English:   match.Name,
					AniListID: mediaID,
				}, nil
			}
		default:
			return Anime{}, fmt.Errorf("AniPub match is ambiguous for AniList %d", mediaID)
		}
	}
	return Anime{}, fmt.Errorf("no conservative AniPub match for AniList %d", mediaID)
}

func (c *Client) search(ctx context.Context, query string) ([]searchResult, error) {
	// The query is a path segment rather than a parameter, so it must be
	// escaped as one; a bare space or slash otherwise changes the route.
	endpoint := c.base + "/api/search/" + url.PathEscape(strings.TrimSpace(query))
	body, err := c.fetchBytes(ctx, endpoint)
	if err != nil {
		return nil, fmt.Errorf("search AniPub: %w", err)
	}
	results, err := decodeSearchResults(body)
	if err != nil {
		return nil, fmt.Errorf("search AniPub: %w", err)
	}
	return results, nil
}

// decodeSearchResults reads a search payload, which is not one shape. The
// endpoint answers with an array for several matches, with the bare object for
// exactly one, and with a {"found": false} marker for none. An exact title
// therefore arrives as an object, so decoding only the array form fails on the
// very case a conservative match depends on.
func decodeSearchResults(body []byte) ([]searchResult, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, nil
	}
	switch trimmed[0] {
	case '[':
		var results []searchResult
		if err := json.Unmarshal(trimmed, &results); err != nil {
			return nil, fmt.Errorf("decode results: %w", err)
		}
		return results, nil
	case '{':
		var single searchResult
		if err := json.Unmarshal(trimmed, &single); err != nil {
			return nil, fmt.Errorf("decode result: %w", err)
		}
		if single.Error != "" {
			return nil, fmt.Errorf("AniPub API: %s", single.Error)
		}
		// The no-match marker carries no id, and an entry without one cannot
		// be resolved anyway.
		if single.ID == 0 {
			return nil, nil
		}
		return []searchResult{single}, nil
	}
	return nil, fmt.Errorf("unexpected search payload")
}

// Episodes reads explicit numbers from /play routes, or the ordered /video
// catalogue whose separately stored first entry is labelled Episode 1.
func (c *Client) Episodes(ctx context.Context, anime Anime, _ string) ([]Episode, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(anime.ID) == "" {
		return nil, errors.New("AniPub episodes: show id is required")
	}
	// The details API has a low per-minute limit. Playback and next-episode
	// prefetch ask for the same list, including across sub/dub lookups.
	c.cacheMu.Lock()
	entry, ok := c.episodeCache[anime.ID]
	c.cacheMu.Unlock()
	if ok && time.Now().Before(entry.expires) {
		return append([]Episode(nil), entry.episodes...), nil
	}
	var details detailsResponse
	endpoint := c.base + "/v1/api/details/" + url.PathEscape(strings.TrimSpace(anime.ID))
	if err := c.fetchJSON(ctx, endpoint, &details); err != nil {
		return nil, fmt.Errorf("list AniPub episodes: %w", err)
	}
	if details.Error != "" {
		return nil, fmt.Errorf("list AniPub episodes: %s", details.Error)
	}

	links := make([]string, 0, len(details.Local.Episodes)+1)
	links = append(links, details.Local.Link)
	for _, item := range details.Local.Episodes {
		links = append(links, item.Link)
	}

	legacy := videoLinkRE.MatchString(details.Local.Link)
	if legacy && !strings.EqualFold(strings.TrimSpace(details.Local.Name), "Episode 1") {
		return nil, errors.New("AniPub legacy episode list does not start at episode one")
	}
	seen := map[int]bool{}
	seenVideos := map[string]bool{}
	episodes := make([]Episode, 0, len(links))
	directShowID, _, direct := parsePlayLink(details.Local.Link)
	for index, link := range links {
		// Older catalogue entries use opaque video IDs, with episode one
		// stored separately and the remaining episodes in playback order.
		// Preserve those IDs rather than treating them as MAL/episode numbers.
		if legacy {
			match := videoLinkRE.FindStringSubmatch(link)
			if match == nil || seenVideos[match[1]] {
				return nil, errors.New("AniPub legacy episode list has missing or duplicate video IDs")
			}
			seenVideos[match[1]] = true
			episodes = append(episodes, Episode{ShowID: anime.ID, Number: index + 1, Value: "video:" + match[1]})
			continue
		}
		show, number, ok := parsePlayLink(link)
		if !ok || show != directShowID {
			direct = false
		}
		if !ok || seen[number] {
			continue
		}
		seen[number] = true
		episodes = append(episodes, Episode{
			ShowID: show,
			Number: number,
			Value:  strconv.Itoa(number),
		})
	}
	if len(episodes) == 0 {
		return nil, fmt.Errorf("no AniPub episodes for %s", anime.ID)
	}
	sort.SliceStable(episodes, func(i, j int) bool { return episodes[i].Number < episodes[j].Number })
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for id, entry := range c.episodeCache {
		if time.Now().After(entry.expires) {
			delete(c.episodeCache, id)
		}
	}
	if !direct {
		directShowID = ""
	}
	c.episodeCache[anime.ID] = episodeEntry{
		episodes: append([]Episode(nil), episodes...), expires: time.Now().Add(time.Minute),
		directShowID: directShowID,
	}
	return episodes, nil
}

// LookupEpisode recovers an explicit /play route when AniPub's catalogue lags
// behind its player. Use only a consistent show ID from the matched catalogue;
// legacy /video IDs and conflicting show IDs cannot be extrapolated. The
// requested route is not added to the catalogue, and Streams must validate it.
func (c *Client) LookupEpisode(ctx context.Context, anime Anime, number int, translation string) (Episode, error) {
	if err := ctx.Err(); err != nil {
		return Episode{}, err
	}
	if number <= 0 {
		return Episode{}, errors.New("AniPub episode lookup requires a positive episode number")
	}
	episodes, err := c.Episodes(ctx, anime, translation)
	if err != nil {
		return Episode{}, err
	}
	for _, episode := range episodes {
		if episode.Number == number {
			return episode, nil
		}
	}
	c.cacheMu.Lock()
	showID := c.episodeCache[anime.ID].directShowID
	c.cacheMu.Unlock()
	if id, err := strconv.Atoi(showID); err != nil || id <= 0 {
		return Episode{}, errors.New("AniPub catalogue has no explicit episode route")
	}
	if err := ctx.Err(); err != nil {
		return Episode{}, err
	}
	return Episode{ShowID: showID, Number: number, Value: strconv.Itoa(number)}, nil
}

// LookupMALEpisode addresses the player's explicit MAL route independently of
// the title catalogue, which can lag behind newly released seasons. A catalogue
// ID is not a MAL ID; this route must never be saved as a title mapping.
func (c *Client) LookupMALEpisode(ctx context.Context, malID, number int, translation string) (Episode, error) {
	if err := ctx.Err(); err != nil {
		return Episode{}, err
	}
	if malID <= 0 || number <= 0 || (translation != "sub" && translation != "dub") {
		return Episode{}, errors.New("AniPub MAL lookup requires positive IDs and a supported translation")
	}
	episode := Episode{ShowID: strconv.Itoa(malID), Number: number, Value: strconv.Itoa(number)}
	embed, _, err := c.playerEmbed(ctx, episode, translation)
	if err != nil {
		return Episode{}, err
	}
	u, err := url.Parse(embed)
	if err != nil || u.Path != fmt.Sprintf("/stream/mal/%d/%d/%s", malID, number, translation) {
		return Episode{}, errors.New("AniPub player does not confirm the requested MAL episode")
	}
	return episode, nil
}

// parsePlayLink reads the play route's show id and episode number. The stored
// value is an attribute fragment rather than a URL, so it is matched rather
// than parsed.
func parsePlayLink(raw string) (show string, number int, ok bool) {
	match := playLinkRE.FindStringSubmatch(raw)
	if match == nil {
		return "", 0, false
	}
	number, err := strconv.Atoi(match[2])
	if err != nil || number <= 0 {
		return "", 0, false
	}
	return match[1], number, true
}

// watchURL builds the player route for an episode. The shape is part of the
// site's routing rather than of its JavaScript, so it survives bundle changes.
func watchURL(base, showID, episodeValue, translation string) string {
	translation = strings.ToLower(strings.TrimSpace(translation))
	if translation == "" {
		translation = "sub"
	}
	if videoID, ok := strings.CutPrefix(episodeValue, "video:"); ok {
		return fmt.Sprintf("%s/video/%s/%s", strings.TrimRight(base, "/"), url.PathEscape(videoID), url.PathEscape(translation))
	}
	return fmt.Sprintf("%s/play/%s/%s/%s",
		strings.TrimRight(base, "/"),
		url.PathEscape(showID),
		url.PathEscape(episodeValue),
		url.PathEscape(translation),
	)
}

// Streams tries native HTTP resolution first, then an optional browser capture.
// Both paths normalize the selected playlist before caching its local URL.
func (c *Client) Streams(ctx context.Context, episode Episode, translation, quality string) (streams []Stream, err error) {
	ctx = streamlog.WithProvider(ctx, "anipub")
	finish := streamlog.Start(ctx, "provider", "streams")
	defer func() {
		if ctx.Err() != nil {
			streams, err = nil, ctx.Err()
		}
		finish(browser.Outcome(ctx, err))
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(episode.ShowID) == "" || episode.Number <= 0 {
		return nil, errors.New("AniPub streams: show id and positive episode number are required")
	}
	if episode.Value == "" {
		episode.Value = strconv.Itoa(episode.Number)
	}
	translation = strings.ToLower(strings.TrimSpace(translation))
	if translation == "" {
		translation = "sub"
	}
	if translation != "sub" && translation != "dub" {
		return nil, fmt.Errorf("unsupported AniPub translation %q", translation)
	}
	c.proxyMu.Lock()
	closed := c.closed
	c.proxyMu.Unlock()
	if closed {
		return nil, errors.New("AniPub client is closed")
	}
	if cached, ok := c.cachedStreams(episode, translation, quality); ok {
		source := cached[0].Source
		if source != "native" {
			source = browser.SourceID(source)
		}
		finishCache := streamlog.Start(ctx, source, "cache")
		finishCache("resolved", "validated_cache")
		return cached, nil
	}
	finishNative := streamlog.Start(ctx, "native", "resolve")
	streams, err = c.nativeStreams(ctx, episode, translation, quality)
	finishNative(streamlog.Outcome(ctx, err))
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil && c.browser != nil {
		nativeErr := err
		streams, err = c.browserStreams(ctx, episode, translation, quality)
		if err == nil {
			var prepared Stream
			for _, stream := range streams {
				prepared, err = c.prepareStream(ctx, stream, quality, nil)
				if err == nil {
					streams = []Stream{prepared}
					break
				}
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
			}
		}
		if err != nil {
			err = errors.Join(nativeErr, err)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("resolve AniPub episode %d: %w", episode.Number, err)
	}
	if err := c.cacheStreams(ctx, episode, translation, quality, streams); err != nil {
		return nil, err
	}
	return streams, nil
}

func (c *Client) fetchJSON(ctx context.Context, endpoint string, out any) error {
	body, err := c.fetchBytes(ctx, endpoint)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func (c *Client) fetchBytes(ctx context.Context, endpoint string) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", userAgent)
	request.Header.Set("Accept", "application/json")
	// The catalogue endpoints answer only to requests that look like they came
	// from the site itself.
	request.Header.Set("Referer", c.base+"/")

	response, err := c.http.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxBody))
	if err != nil {
		return nil, err
	}
	return body, nil
}

func uniqueTitles(aliases []string) []string {
	seen := map[string]bool{}
	titles := make([]string, 0, len(aliases))
	for _, alias := range aliases {
		alias = strings.TrimSpace(alias)
		if alias == "" {
			continue
		}
		key := normalizeTitle(alias)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		titles = append(titles, alias)
	}
	return titles
}

// normalizeTitle reduces a title to letters and digits separated by single
// spaces, so punctuation and casing differences between the tracker and the
// catalogue do not defeat an otherwise exact match.
func normalizeTitle(value string) string {
	var builder strings.Builder
	space := false
	for _, r := range strings.ToLower(value) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			if space && builder.Len() > 0 {
				builder.WriteByte(' ')
			}
			space = false
			builder.WriteRune(r)
		default:
			space = true
		}
	}
	return builder.String()
}
