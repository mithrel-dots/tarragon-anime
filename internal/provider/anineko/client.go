// Package anineko resolves anime from anineko.to. The catalogue and episode
// pages are ordinary HTTP, while the two embed hosts need small, provider-local
// adapters before their HLS can be handed to mpv.
package anineko

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
	"tarragon-anime/internal/streamlog"
)

const (
	defaultBaseURL = "https://anineko.to"
	userAgent      = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/153.0.0.0 Safari/537.36"
	maxBody        = 8 << 20
)

var (
	episodePathRE = regexp.MustCompile(`/ep-(\d+)`)
	watchPathRE   = regexp.MustCompile(`/watch/([^/?#]+)`)
)

type (
	Anime   = provider.Anime
	Episode = provider.Episode
	Stream  = provider.Stream
)

var _ provider.Client = (*Client)(nil)

// Client talks to AniNeko's native pages and embed hosts.
type Client struct {
	http    *http.Client
	base    string
	logger  *log.Logger
	proxy   *vibeProxy
	proxyMu sync.Mutex
}

// NewClient returns a client using the public AniNeko origin.
func NewClient(httpClient *http.Client) *Client {
	return NewClientWithBaseURL(httpClient, defaultBaseURL)
}

// NewClientWithBaseURL points the client at an alternate origin, which keeps
// the metadata and page-scrape tests independent of the live site.
func NewClientWithBaseURL(httpClient *http.Client, baseURL string) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	return &Client{
		http:   httpClient,
		base:   strings.TrimRight(baseURL, "/"),
		logger: log.New(io.Discard, "", 0),
	}
}

// WithLogger attaches diagnostics for embed attempts. A failed subtitle or
// alternate embed should not hide a playable stream from the fallback stack.
func (c *Client) WithLogger(logger *log.Logger) *Client {
	if logger != nil {
		c.logger = logger
	}
	return c
}

// Close releases the local segment proxy, if a vibeplayer stream created one.
func (c *Client) Close() {
	c.proxyMu.Lock()
	proxy := c.proxy
	c.proxy = nil
	c.proxyMu.Unlock()
	if proxy != nil {
		proxy.close()
	}
}

type searchResponse struct {
	Success bool `json:"success"`
	Results []struct {
		Title string `json:"title"`
		URL   string `json:"url"`
		Image string `json:"image"`
		Meta  string `json:"meta"`
	} `json:"results"`
}

type searchResult struct {
	Title string
	URL   string
	Image string
	Meta  string
}

type embedPage struct {
	URL  string
	HTML string
}

// Match finds a conservative exact title match. AniNeko uses slugs as ids and
// does not expose AniList ids, so aliases from AniList are tried in order.
func (c *Client) Match(ctx context.Context, mediaID int, aliases []string, _ string) (Anime, error) {
	for _, query := range uniqueTitles(aliases) {
		results, err := c.search(ctx, query)
		if err != nil {
			return Anime{}, err
		}
		matches := map[string]searchResult{}
		for _, result := range results {
			slug := slugFromWatchURL(result.URL)
			if slug == "" || normalizeTitle(result.Title) != normalizeTitle(query) {
				continue
			}
			matches[slug] = result
		}
		switch len(matches) {
		case 0:
			continue
		case 1:
			for slug, result := range matches {
				name := strings.TrimSpace(result.Title)
				return Anime{
					ID:        slug,
					Name:      name,
					English:   name,
					AniListID: mediaID,
				}, nil
			}
		default:
			return Anime{}, fmt.Errorf("AniNeko match is ambiguous for AniList %d", mediaID)
		}
	}
	return Anime{}, fmt.Errorf("no conservative AniNeko match for AniList %d", mediaID)
}

func (c *Client) search(ctx context.Context, query string) ([]searchResult, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errors.New("AniNeko search query is empty")
	}
	endpoint := c.base + "/ajax/search?q=" + url.QueryEscape(query)
	var payload searchResponse
	if err := c.fetchJSON(ctx, http.MethodGet, endpoint, c.base+"/", nil, &payload); err != nil {
		return nil, fmt.Errorf("search AniNeko: %w", err)
	}
	if !payload.Success {
		return nil, errors.New("AniNeko search failed")
	}
	results := make([]searchResult, 0, len(payload.Results))
	for _, item := range payload.Results {
		results = append(results, searchResult{
			Title: strings.TrimSpace(item.Title),
			URL:   strings.TrimSpace(item.URL),
			Image: strings.TrimSpace(item.Image),
			Meta:  strings.TrimSpace(item.Meta),
		})
	}
	return results, nil
}

// Episodes extracts episode numbers from the show's watch page. The page is
// the source of truth because AniNeko does not expose a separate episode API.
func (c *Client) Episodes(ctx context.Context, anime Anime, _ string) ([]Episode, error) {
	slug := strings.TrimSpace(anime.ID)
	if slug == "" {
		return nil, errors.New("AniNeko episodes: show id is required")
	}
	html, err := c.fetchString(ctx, c.watchURL(slug, 0), c.base+"/")
	if err != nil {
		return nil, fmt.Errorf("list AniNeko episodes: %w", err)
	}
	seen := map[int]bool{}
	for _, match := range episodePathRE.FindAllStringSubmatch(html, -1) {
		if len(match) < 2 {
			continue
		}
		number, parseErr := strconv.Atoi(match[1])
		if parseErr == nil && number > 0 {
			seen[number] = true
		}
	}
	if len(seen) == 0 {
		return nil, fmt.Errorf("AniNeko returned no episodes for %q", slug)
	}
	numbers := make([]int, 0, len(seen))
	for number := range seen {
		numbers = append(numbers, number)
	}
	sort.Ints(numbers)
	episodes := make([]Episode, 0, len(numbers))
	for _, number := range numbers {
		episodes = append(episodes, Episode{
			ShowID: slug,
			Number: number,
			Value:  strconv.Itoa(number),
		})
	}
	return episodes, nil
}

// Streams resolves the first playable embed for the requested translation.
// Soft subtitles use the embed's external track when available; hard subtitles
// remain burned into the selected video stream.
func (c *Client) Streams(ctx context.Context, episode Episode, translation, quality string) ([]Stream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	slug := strings.TrimSpace(episode.ShowID)
	if slug == "" {
		return nil, errors.New("AniNeko streams: show id is required")
	}
	if episode.Number <= 0 {
		return nil, fmt.Errorf("AniNeko streams: invalid episode number %d", episode.Number)
	}
	page, err := c.loadEmbedPage(ctx, slug, episode.Number)
	if err != nil {
		return nil, fmt.Errorf("load AniNeko episode %d: %w", episode.Number, err)
	}
	groups := extractLangEmbedURLs(page.HTML)
	embedURLs := embedURLsForMode(groups, translation)
	if len(embedURLs) == 0 {
		return nil, fmt.Errorf("no AniNeko %s streams for episode %d", normalizeTranslation(translation), episode.Number)
	}

	var failures []error
	seen := make(map[string]bool)
	for _, embedURL := range embedURLs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		embedURL = c.absoluteURL(embedURL)
		if seen[embedURL] {
			continue
		}
		seen[embedURL] = true
		id := fmt.Sprintf("source-%d", len(seen))
		finish := streamlog.Start(ctx, id, "resolve")
		if resolveEmbedHost(embedURL) == "" {
			finish("unavailable", "unsupported_source")
			continue
		}
		stream, resolveErr := c.resolveEmbed(ctx, embedURL, quality)
		if ctx.Err() != nil {
			resolveErr = ctx.Err()
		}
		finish(streamlog.Outcome(ctx, resolveErr))
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if resolveErr != nil {
			failures = append(failures, resolveErr)
			continue
		}
		stream.Source = id
		return []Stream{stream}, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(failures) == 0 {
		return nil, fmt.Errorf("no playable AniNeko streams for episode %d", episode.Number)
	}
	return nil, fmt.Errorf("no playable AniNeko streams for episode %d: %w", episode.Number, errors.Join(failures...))
}

func (c *Client) loadEmbedPage(ctx context.Context, slug string, number int) (embedPage, error) {
	html, err := c.fetchString(ctx, c.watchURL(slug, number), c.base+"/")
	if err != nil {
		return embedPage{}, err
	}
	return embedPage{URL: c.watchURL(slug, number), HTML: html}, nil
}

func (c *Client) resolveEmbed(ctx context.Context, embedURL, quality string) (Stream, error) {
	if err := ctx.Err(); err != nil {
		return Stream{}, err
	}
	embedURL = c.absoluteURL(embedURL)
	switch resolveEmbedHost(embedURL) {
	case "bibiemb":
		return c.resolveBibiemb(ctx, embedURL, quality)
	case "vibeplayer":
		return c.resolveVibeplayer(ctx, embedURL)
	default:
		return Stream{}, fmt.Errorf("unsupported AniNeko embed host %q", embedURL)
	}
}

func (c *Client) fetchString(ctx context.Context, endpoint, referer string) (string, error) {
	body, err := c.fetchBytes(ctx, endpoint, referer)
	return string(body), err
}

func (c *Client) fetchBytes(ctx context.Context, endpoint, referer string) ([]byte, error) {
	return c.requestBody(ctx, http.MethodGet, endpoint, referer, nil)
}

// fetchJSON is used only for JSON endpoints. Pages and playlists use the same
// request path but are decoded as raw bytes by fetchBytes.
func (c *Client) fetchJSON(ctx context.Context, method, endpoint, referer string, payload any, out any) error {
	body, err := c.requestBody(ctx, method, endpoint, referer, payload)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode response: %w: %s", err, compactBody(body))
	}
	return nil
}

func (c *Client) requestBody(ctx context.Context, method, endpoint, referer string, payload any) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var reader io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, err
	}
	setHeaders(request, referer)
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxBody))
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("HTTP status %s: %s", response.Status, compactBody(body))
	}
	return body, nil
}

func setHeaders(request *http.Request, referer string) {
	request.Header.Set("User-Agent", userAgent)
	request.Header.Set("Accept", "text/html,application/json;q=0.9,*/*;q=0.8")
	if referer != "" {
		request.Header.Set("Referer", referer)
	}
}

func compactBody(body []byte) string {
	value := strings.Join(strings.Fields(string(body)), " ")
	if len(value) > 240 {
		return value[:240] + "..."
	}
	return value
}

func (c *Client) watchURL(slug string, number int) string {
	if number > 0 {
		return fmt.Sprintf("%s/watch/%s/ep-%d", c.base, url.PathEscape(slug), number)
	}
	return fmt.Sprintf("%s/watch/%s", c.base, url.PathEscape(slug))
}

func (c *Client) absoluteURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
		return raw
	}
	if strings.HasPrefix(raw, "//") {
		return "https:" + raw
	}
	if !strings.HasPrefix(raw, "/") {
		raw = "/" + raw
	}
	return c.base + raw
}

func slugFromWatchURL(raw string) string {
	match := watchPathRE.FindStringSubmatch(raw)
	if len(match) < 2 {
		return ""
	}
	return strings.TrimSpace(match[1])
}

func uniqueTitles(aliases []string) []string {
	seen := map[string]bool{}
	result := make([]string, 0, len(aliases))
	for _, alias := range aliases {
		alias = strings.TrimSpace(alias)
		key := normalizeTitle(alias)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		result = append(result, alias)
	}
	return result
}

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
