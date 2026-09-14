// Package anipub resolves anime from AniPub. Metadata comes from the site's
// own JSON endpoints; playable URLs come from a browser session, because the
// player's source response carries the media URL encrypted and only the site's
// JavaScript can unwrap it. What the player then requests over the network is
// an ordinary HLS master playlist with a separate subtitle track, so the
// browser only has to watch rather than decrypt.
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

	"tarragon-anime/internal/provider"
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

// ErrBrowserRequired reports that stream resolution was attempted without a
// browser. The player encrypts its source response, so there is no HTTP-only
// path to a playable URL.
var ErrBrowserRequired = errors.New("AniPub stream resolution requires a browser session")

// playLinkRE pulls the play route out of a details entry. The site stores the
// value as an iframe attribute fragment rather than a bare URL, and the
// episode number is a path segment inside it.
var playLinkRE = regexp.MustCompile(`/play/(\d+)/(\d+)(?:/([a-zA-Z]+))?`)

type Client struct {
	http   *http.Client
	base   string
	logger *log.Logger

	browser Resolver

	cacheMu     sync.Mutex
	streamCache map[string]streamEntry
}

var _ provider.Client = (*Client)(nil)

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
		http:        httpClient,
		base:        strings.TrimRight(baseURL, "/"),
		logger:      log.New(io.Discard, "", 0),
		streamCache: map[string]streamEntry{},
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
}

type detailsResponse struct {
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
		// The no-match marker carries no id, and an entry without one cannot
		// be resolved anyway.
		if single.ID == 0 {
			return nil, nil
		}
		return []searchResult{single}, nil
	}
	return nil, fmt.Errorf("unexpected search payload")
}

// Episodes lists the show's episodes. Numbers come from the play route rather
// than from the list position, because the list omits the first episode and
// carries no numbering of its own.
func (c *Client) Episodes(ctx context.Context, anime Anime, _ string) ([]Episode, error) {
	if strings.TrimSpace(anime.ID) == "" {
		return nil, errors.New("AniPub episodes: show id is required")
	}
	var details detailsResponse
	endpoint := c.base + "/v1/api/details/" + url.PathEscape(strings.TrimSpace(anime.ID))
	if err := c.fetchJSON(ctx, endpoint, &details); err != nil {
		return nil, fmt.Errorf("list AniPub episodes: %w", err)
	}

	links := make([]string, 0, len(details.Local.Episodes)+1)
	links = append(links, details.Local.Link)
	for _, item := range details.Local.Episodes {
		links = append(links, item.Link)
	}

	seen := map[int]bool{}
	episodes := make([]Episode, 0, len(links))
	for _, link := range links {
		show, number, ok := parsePlayLink(link)
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
	return episodes, nil
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
	return fmt.Sprintf("%s/play/%s/%s/%s",
		strings.TrimRight(base, "/"),
		url.PathEscape(showID),
		url.PathEscape(episodeValue),
		url.PathEscape(translation),
	)
}

// Streams resolves playable URLs for an episode. Results are cached briefly
// because the captured URLs are signed and a capture costs a page load.
func (c *Client) Streams(ctx context.Context, episode Episode, translation, quality string) ([]Stream, error) {
	if strings.TrimSpace(episode.ShowID) == "" {
		return nil, errors.New("AniPub streams: show id is required")
	}
	if cached, ok := c.cachedStreams(episode, translation, quality); ok {
		return cached, nil
	}
	if c.browser == nil {
		return nil, ErrBrowserRequired
	}
	streams, err := c.browserStreams(ctx, episode, translation, quality)
	if err != nil {
		return nil, fmt.Errorf("resolve AniPub episode %d: %w", episode.Number, err)
	}
	c.cacheStreams(episode, translation, quality, streams)
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
