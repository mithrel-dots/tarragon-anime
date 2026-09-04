package animepahe

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"

	"tarragon-anime/internal/provider"
)

const (
	defaultBaseURL = "https://animepahe.pw"
	userAgent      = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"
)

var (
	buttonRE = regexp.MustCompile(`(?is)<button\b[^>]*>`)
	attrRE   = regexp.MustCompile(`(?i)(data-src|data-audio|data-resolution)\s*=\s*["']([^"']*)["']`)
	packerRE = regexp.MustCompile(`eval\(function\(p,a,c,k,e,d\).*?\}\('([^']*)',([0-9]+),([0-9]+),'([^']*)'`)
	m3u8RE   = regexp.MustCompile(`https?://[^\s"'\\]+\.m3u8[^\s"'\\]*`)
)

type Client struct {
	http *http.Client
	base string

	accessMu sync.Mutex
	bypassed bool
	solver   func() ([]*http.Cookie, error)
}

var _ provider.Client = (*Client)(nil)

func NewClient(httpClient *http.Client) *Client {
	return NewClientWithBaseURL(httpClient, defaultBaseURL)
}

func NewClientWithBaseURL(httpClient *http.Client, baseURL string) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	client := *httpClient
	if client.Jar == nil {
		client.Jar, _ = cookiejar.New(nil)
	}
	return &Client{
		http:   &client,
		base:   strings.TrimRight(baseURL, "/"),
		solver: func() ([]*http.Cookie, error) { return solveChallenge(baseURL) },
	}
}

type searchResponse struct {
	Data []struct {
		ID      int    `json:"id"`
		Title   string `json:"title"`
		Session string `json:"session"`
	} `json:"data"`
}

type releaseResponse struct {
	CurrentPage int `json:"current_page"`
	LastPage    int `json:"last_page"`
	Data        []struct {
		Episode int    `json:"episode"`
		Session string `json:"session"`
	} `json:"data"`
}

type searchItem struct {
	ID      int
	Title   string
	Session string
}

type animeRef struct {
	releaseID string
	session   string
}

func parseAnimeID(value string) animeRef {
	value = strings.TrimSpace(value)
	parts := strings.SplitN(value, ":", 2)
	if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
		if _, err := strconv.Atoi(parts[0]); err == nil {
			return animeRef{releaseID: parts[0], session: parts[1]}
		}
	}
	if _, err := strconv.Atoi(value); err == nil {
		return animeRef{releaseID: value}
	}
	return animeRef{session: value}
}

func (r animeRef) apiID() string {
	if r.session != "" {
		return r.session
	}
	return r.releaseID
}

func formatAnimeID(id int, session string) string {
	if id > 0 && session != "" {
		return fmt.Sprintf("%d:%s", id, session)
	}
	if session != "" {
		return session
	}
	return strconv.Itoa(id)
}

func (c *Client) Match(ctx context.Context, mediaID int, aliases []string, _ string) (provider.Anime, error) {
	matches := make(map[string]provider.Anime)
	for _, query := range uniqueTitles(aliases) {
		items, err := c.search(ctx, query)
		if err != nil {
			return provider.Anime{}, err
		}
		for _, item := range items {
			candidate := provider.Anime{ID: formatAnimeID(item.ID, item.Session), Name: item.Title, AniListID: mediaID}
			if candidate.ID != "" && normalizeTitle(candidate.Name) == normalizeTitle(query) {
				matches[candidate.ID] = candidate
			}
		}
		if len(matches) > 0 {
			break
		}
	}
	if len(matches) == 1 {
		for _, match := range matches {
			return match, nil
		}
	}
	if len(matches) > 1 {
		return provider.Anime{}, fmt.Errorf("AnimePahe match is ambiguous for AniList %d", mediaID)
	}
	return provider.Anime{}, fmt.Errorf("no conservative AnimePahe match for AniList %d", mediaID)
}

func (c *Client) search(ctx context.Context, query string) ([]searchItem, error) {
	body, err := c.get(ctx, c.base+"/api?m=search&q="+url.QueryEscape(query), false)
	if err != nil {
		return nil, fmt.Errorf("search AnimePahe: %w", err)
	}
	var payload searchResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("parse AnimePahe search: %w", err)
	}
	result := make([]searchItem, 0, len(payload.Data))
	for _, item := range payload.Data {
		result = append(result, searchItem{ID: item.ID, Title: item.Title, Session: item.Session})
	}
	return result, nil
}

func (c *Client) Episodes(ctx context.Context, anime provider.Anime, _ string) ([]provider.Episode, error) {
	ref := parseAnimeID(anime.ID)
	if ref.apiID() == "" {
		return nil, fmt.Errorf("AnimePahe provider id %q is invalid", anime.ID)
	}
	var result []provider.Episode
	for page := 1; ; page++ {
		endpoint := fmt.Sprintf("%s/api?m=release&id=%s&sort=episode_asc&page=%d", c.base, url.QueryEscape(ref.apiID()), page)
		body, err := c.get(ctx, endpoint, false)
		if err != nil {
			return nil, fmt.Errorf("list AnimePahe episodes: %w", err)
		}
		var payload releaseResponse
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, fmt.Errorf("parse AnimePahe episodes: %w", err)
		}
		for _, item := range payload.Data {
			if item.Episode > 0 {
				result = append(result, provider.Episode{ShowID: anime.ID, Number: item.Episode, Value: item.Session})
			}
		}
		if payload.LastPage == 0 || payload.CurrentPage >= payload.LastPage {
			break
		}
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].Number < result[j].Number })
	if len(result) == 0 {
		return nil, fmt.Errorf("AnimePahe returned no episodes")
	}
	return result, nil
}

func (c *Client) Streams(ctx context.Context, episode provider.Episode, translation, _ string) ([]provider.Stream, error) {
	ref := parseAnimeID(episode.ShowID)
	if ref.session == "" {
		return nil, fmt.Errorf("AnimePahe provider id %q is missing a session id", episode.ShowID)
	}
	if episode.Value == "" {
		return nil, fmt.Errorf("AnimePahe episode %d is missing a session id", episode.Number)
	}
	body, err := c.get(ctx, fmt.Sprintf("%s/play/%s/%s", c.base, ref.session, episode.Value), true)
	if err != nil {
		return nil, fmt.Errorf("load AnimePahe player page: %w", err)
	}
	links := parsePlayerLinks(string(body), translation)
	if len(links) == 0 {
		return nil, fmt.Errorf("AnimePahe returned no %s streams for episode %d", translation, episode.Number)
	}
	var result []provider.Stream
	for _, link := range links {
		streamURL, err := c.extractKwik(ctx, link)
		if err == nil {
			result = append(result, provider.Stream{URL: streamURL, Headers: map[string]string{"Referer": c.base + "/"}})
		}
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("extract AnimePahe streams from Kwik")
	}
	return result, nil
}

type playerLink struct {
	url string
	res int
}

func parsePlayerLinks(html, translation string) []string {
	var sub, dub []playerLink
	for _, button := range buttonRE.FindAllString(html, -1) {
		attributes := make(map[string]string)
		for _, match := range attrRE.FindAllStringSubmatch(button, -1) {
			attributes[strings.ToLower(match[1])] = match[2]
		}
		if !strings.Contains(attributes["data-src"], "kwik.cx") {
			continue
		}
		item := playerLink{url: attributes["data-src"]}
		item.res, _ = strconv.Atoi(attributes["data-resolution"])
		if strings.EqualFold(attributes["data-audio"], "eng") {
			dub = append(dub, item)
		} else {
			sub = append(sub, item)
		}
	}
	sort.SliceStable(sub, func(i, j int) bool { return sub[i].res > sub[j].res })
	sort.SliceStable(dub, func(i, j int) bool { return dub[i].res > dub[j].res })
	selected := sub
	if strings.EqualFold(translation, "dub") {
		selected = dub
	}
	result := make([]string, 0, len(selected))
	for _, link := range selected {
		result = append(result, link.url)
	}
	return result
}

func (c *Client) extractKwik(ctx context.Context, link string) (string, error) {
	body, err := c.get(ctx, link, true)
	if err != nil {
		return "", err
	}
	for _, match := range packerRE.FindAllStringSubmatch(string(body), -1) {
		base, _ := strconv.Atoi(match[2])
		count, _ := strconv.Atoi(match[3])
		words := strings.Split(match[4], "|")
		if base <= 1 || count <= 0 || count > len(words) {
			continue
		}
		if streamURL := m3u8RE.FindString(unpackPacker(match[1], base, count, words)); streamURL != "" {
			return streamURL, nil
		}
	}
	return "", fmt.Errorf("m3u8 link not found in Kwik page")
}

func unpackPacker(value string, base, count int, words []string) string {
	encode := func(value int) string {
		if value == 0 {
			return "0"
		}
		var result string
		for value > 0 {
			digit := value % base
			if digit > 35 {
				result = string(rune(digit+29)) + result
			} else {
				result = strconv.FormatInt(int64(digit), 36) + result
			}
			value /= base
		}
		return result
	}
	replacements := make(map[string]string, count)
	for index := count - 1; index >= 0; index-- {
		word := words[index]
		if word == "" {
			word = encode(index)
		}
		replacements[encode(index)] = word
	}
	return regexp.MustCompile(`[A-Za-z0-9_]+`).ReplaceAllStringFunc(value, func(word string) string {
		if replacement := replacements[word]; replacement != "" {
			return replacement
		}
		return word
	})
}

func (c *Client) get(ctx context.Context, endpoint string, page bool) ([]byte, error) {
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", userAgent)
		req.Header.Set("Referer", c.base+"/")
		if page {
			req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
		} else {
			req.Header.Set("Accept", "application/json, text/javascript, */*; q=0.01")
			req.Header.Set("X-Requested-With", "XMLHttpRequest")
		}
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, err
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		if isChallenge(resp, body) {
			if attempt == 1 {
				return nil, fmt.Errorf("AnimePahe request remains blocked by DDoS-Guard")
			}
			if err := c.bypass(); err != nil {
				return nil, err
			}
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, fmt.Errorf("AnimePahe HTTP status %s", resp.Status)
		}
		return body, nil
	}
	return nil, fmt.Errorf("AnimePahe request failed")
}

func (c *Client) bypass() error {
	c.accessMu.Lock()
	defer c.accessMu.Unlock()
	if c.bypassed {
		return nil
	}
	if c.solver == nil {
		return fmt.Errorf("AnimePahe DDoS-Guard challenge requires Chromium")
	}
	cookies, err := c.solver()
	if err != nil {
		return fmt.Errorf("solve AnimePahe DDoS-Guard challenge: %w", err)
	}
	parsed, err := url.Parse(c.base)
	if err != nil {
		return err
	}
	c.http.Jar.SetCookies(parsed, cookies)
	c.bypassed = true
	return nil
}

func solveChallenge(baseURL string) ([]*http.Cookie, error) {
	var result []*http.Cookie
	err := rod.Try(func() {
		browserLauncher := launcher.New().Headless(true)
		defer browserLauncher.Cleanup()
		browser := rod.New().ControlURL(browserLauncher.MustLaunch()).MustConnect()
		defer browser.MustClose()
		page := browser.MustPage(strings.TrimRight(baseURL, "/") + "/")
		page.MustWaitLoad()
		for range 30 {
			info, err := page.Info()
			if err == nil && info.Title != "DDoS-Guard" && info.Title != "Just a moment..." && info.Title != "" {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		cookies, err := page.Cookies(nil)
		if err != nil {
			panic(err)
		}
		for _, cookie := range cookies {
			result = append(result, &http.Cookie{Name: cookie.Name, Value: cookie.Value})
		}
	})
	return result, err
}

func isChallenge(resp *http.Response, body []byte) bool {
	value := strings.ToLower(string(body))
	server := strings.ToLower(resp.Header.Get("Server"))
	return strings.Contains(value, "ddos-guard") || strings.Contains(value, "checking your browser") ||
		strings.Contains(value, "just a moment") || strings.Contains(value, "/cdn-cgi/challenge-platform/") ||
		(resp.StatusCode == http.StatusForbidden && (strings.Contains(server, "ddos-guard") || strings.Contains(server, "cloudflare")))
}

func uniqueTitles(values []string) []string {
	seen := make(map[string]bool)
	var result []string
	for _, value := range values {
		normalized := normalizeTitle(value)
		if normalized == "" || seen[normalized] {
			continue
		}
		seen[normalized] = true
		result = append(result, strings.TrimSpace(value))
	}
	return result
}

func normalizeTitle(value string) string {
	var words []string
	var word strings.Builder
	flush := func() {
		if word.Len() > 0 {
			words = append(words, word.String())
			word.Reset()
		}
	}
	for _, r := range strings.ToLower(value) {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			word.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return strings.Join(words, " ")
}
