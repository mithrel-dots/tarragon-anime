package allanime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"tarragon-anime/internal/provider"
)

const (
	defaultAPI    = "https://api.mkissa.net/api"
	defaultOrigin = "https://mkissa.to"
	// buildID identifies the site build to the metadata API. Search and
	// episode listing only need it echoed back in a header; unlike the
	// retired stream signing it carries no key material.
	buildID   = "162"
	userAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"
)

const searchQuery = `query($search:SearchInput,$limit:Int,$page:Int,$translationType:VaildTranslationTypeEnumType,$countryOrigin:VaildCountryOriginEnumType){shows(search:$search,limit:$limit,page:$page,translationType:$translationType,countryOrigin:$countryOrigin){edges{_id name englishName aniListId availableEpisodes}}}`
const episodesQuery = `query($showId:String!){show(_id:$showId){_id availableEpisodesDetail}}`

// ErrBrowserRequired reports that stream resolution has no way to run. Streams
// are read out of the site's own player, so a working browser session is not
// optional.
var ErrBrowserRequired = errors.New("AllAnime stream resolution requires a browser session")

// Aliases preserve the provider's existing public model names while keeping
// the service dependent on the provider-neutral contract.
type Anime = provider.Anime
type Episode = provider.Episode
type Stream = provider.Stream

// Client talks to AllAnime. Metadata (search, episode lists) comes from the
// site's GraphQL API; playable URLs come from a browser session, because the
// site signs and encrypts its source responses with material that only its own
// JavaScript can produce.
type Client struct {
	http   *http.Client
	api    string
	origin string
	now    func() time.Time

	browser Resolver
	logger  *log.Logger

	cacheMu     sync.Mutex
	streamCache map[string]streamEntry
}

func NewClient(httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{
		http: httpClient, api: defaultAPI, origin: defaultOrigin, now: time.Now,
		logger:      log.New(io.Discard, "", 0),
		streamCache: make(map[string]streamEntry),
	}
}

func NewClientWithEndpoints(httpClient *http.Client, api, origin string) *Client {
	c := NewClient(httpClient)
	c.api, c.origin = api, origin
	return c
}

// WithLogger reports how a stream was resolved.
func (c *Client) WithLogger(logger *log.Logger) *Client {
	if logger != nil {
		c.logger = logger
	}
	return c
}

func (c *Client) Search(ctx context.Context, query, translation string) ([]Anime, error) {
	variables := map[string]any{
		"search": map[string]any{"query": query, "allowAdult": false, "allowUnknown": false},
		"limit":  40, "page": 1, "translationType": translation, "countryOrigin": "ALL",
	}
	var data struct {
		Shows struct {
			Edges []struct {
				ID        string `json:"_id"`
				Name      string `json:"name"`
				English   string `json:"englishName"`
				AniListID any    `json:"aniListId"`
			} `json:"edges"`
		} `json:"shows"`
	}
	if err := c.graphQL(ctx, searchQuery, variables, &data); err != nil {
		return nil, fmt.Errorf("search AllAnime: %w", err)
	}
	result := make([]Anime, 0, len(data.Shows.Edges))
	for _, edge := range data.Shows.Edges {
		result = append(result, Anime{
			ID: edge.ID, Name: edge.Name, English: edge.English,
			AniListID: numericID(edge.AniListID),
		})
	}
	return result, nil
}

func (c *Client) Episodes(ctx context.Context, anime Anime, translation string) ([]Episode, error) {
	var data struct {
		Show struct {
			Detail json.RawMessage `json:"availableEpisodesDetail"`
		} `json:"show"`
	}
	if err := c.graphQL(ctx, episodesQuery, map[string]any{"showId": anime.ID}, &data); err != nil {
		return nil, fmt.Errorf("list AllAnime episodes: %w", err)
	}
	values, err := episodeValues(data.Show.Detail, translation)
	if err != nil {
		return nil, fmt.Errorf("parse AllAnime episodes: %w", err)
	}
	result := make([]Episode, 0, len(values))
	for _, value := range values {
		number, err := strconv.Atoi(value)
		if err != nil || number <= 0 {
			continue
		}
		result = append(result, Episode{ShowID: anime.ID, Number: number, Value: value})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Number < result[j].Number })
	return result, nil
}

// Streams resolves playable URLs by reading them off the site's own player in
// a managed browser session. Results are cached per episode, translation and
// quality; see cache.go.
func (c *Client) Streams(ctx context.Context, episode Episode, translation, quality string) ([]Stream, error) {
	if streams, ok := c.cachedStreams(episode, translation, quality); ok {
		return streams, nil
	}
	if c.browser == nil {
		return nil, ErrBrowserRequired
	}
	streams, err := c.browserStreams(ctx, episode, translation, quality)
	if err != nil {
		return nil, fmt.Errorf("resolve AllAnime episode %d: %w", episode.Number, err)
	}
	c.cacheStreams(episode, translation, quality, streams)
	return streams, nil
}

func (c *Client) graphQL(ctx context.Context, query string, variables map[string]any, target any) error {
	body, err := json.Marshal(map[string]any{"query": query, "variables": variables})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.api, bytes.NewReader(body))
	if err != nil {
		return err
	}
	c.webHeaders(req)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-build-id", buildID)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("GraphQL HTTP status %s", resp.Status)
	}
	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&envelope); err != nil {
		return err
	}
	if len(envelope.Errors) > 0 {
		return fmt.Errorf("GraphQL: %s", envelope.Errors[0].Message)
	}
	return json.Unmarshal(envelope.Data, target)
}

func (c *Client) webHeaders(req *http.Request) {
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", c.origin)
	req.Header.Set("Referer", c.origin+"/")
	req.Header.Set("User-Agent", userAgent)
}

func episodeValues(raw json.RawMessage, translation string) ([]string, error) {
	var details map[string]json.RawMessage
	if err := json.Unmarshal(raw, &details); err != nil {
		return nil, err
	}
	selected, ok := details[translation]
	if !ok {
		return nil, fmt.Errorf("no %s episodes available", translation)
	}
	var values []any
	decoder := json.NewDecoder(bytes.NewReader(selected))
	decoder.UseNumber()
	if err := decoder.Decode(&values); err != nil {
		return nil, err
	}
	result := make([]string, 0, len(values))
	for _, value := range values {
		switch value := value.(type) {
		case string:
			result = append(result, value)
		case json.Number:
			result = append(result, value.String())
		}
	}
	return result, nil
}

func numericID(value any) int {
	switch value := value.(type) {
	case float64:
		return int(value)
	case string:
		n, _ := strconv.Atoi(value)
		return n
	default:
		return 0
	}
}
