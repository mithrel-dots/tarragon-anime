package anilist

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

const defaultEndpoint = "https://graphql.anilist.co"

// TokenSource returns the current AniList access token, or an empty string
// when the user is signed out.
type TokenSource func() (string, error)

type Client struct {
	endpoint string
	http     *http.Client
	token    TokenSource
}

type Media struct {
	ID       int
	Title    string
	English  string
	Romaji   string
	Native   string
	Synonyms []string
	Format   string
	Episodes int
	CoverURL string
}

func NewClient(httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{endpoint: defaultEndpoint, http: httpClient}
}

func NewClientWithEndpoint(httpClient *http.Client, endpoint string) *Client {
	c := NewClient(httpClient)
	c.endpoint = endpoint
	return c
}

func NewAuthenticatedClient(httpClient *http.Client, token TokenSource) *Client {
	c := NewClient(httpClient)
	c.token = token
	return c
}

func NewAuthenticatedClientWithEndpoint(httpClient *http.Client, endpoint string, token TokenSource) *Client {
	c := NewClientWithEndpoint(httpClient, endpoint)
	c.token = token
	return c
}

// accessToken reports the current token, requiring the user to be signed in.
func (c *Client) accessToken() (string, error) {
	if c.token == nil {
		return "", fmt.Errorf("AniList authentication is not configured")
	}
	token, err := c.token()
	if err != nil {
		return "", err
	}
	if token == "" {
		return "", fmt.Errorf("not signed in to AniList")
	}
	return token, nil
}

type ListEntry struct {
	ID       int
	Status   string
	Progress int
}

type ListItem struct {
	Media    Media
	Status   string
	Progress int
}

type Viewer struct {
	ID   int
	Name string
}

const mediaFields = `_id: id title { romaji english native } synonyms format episodes coverImage { large }`

func (c *Client) Search(ctx context.Context, query string) ([]Media, error) {
	const gql = `query ($search: String!) { Page(page: 1, perPage: 20) { media(search: $search, type: ANIME, isAdult: false, sort: SEARCH_MATCH) { ` + mediaFields + ` } } }`
	var response struct {
		Page struct {
			Media []mediaResponse `json:"media"`
		} `json:"Page"`
	}
	if err := c.do(ctx, gql, map[string]any{"search": query}, &response); err != nil {
		return nil, fmt.Errorf("search AniList: %w", err)
	}
	media := make([]Media, 0, len(response.Page.Media))
	for _, item := range response.Page.Media {
		media = append(media, item.toMedia())
	}
	return media, nil
}

func (c *Client) Get(ctx context.Context, id int) (Media, error) {
	const gql = `query ($id: Int!) { Media(id: $id, type: ANIME) { ` + mediaFields + ` } }`
	var response struct {
		Media *mediaResponse `json:"Media"`
	}
	if err := c.do(ctx, gql, map[string]any{"id": id}, &response); err != nil {
		return Media{}, fmt.Errorf("get AniList media %d: %w", id, err)
	}
	if response.Media == nil {
		return Media{}, fmt.Errorf("get AniList media %d: not found", id)
	}
	return response.Media.toMedia(), nil
}

func (c *Client) Viewer(ctx context.Context) (Viewer, error) {
	const gql = `query { Viewer { id name } }`
	var response struct {
		Viewer Viewer `json:"Viewer"`
	}
	if err := c.do(ctx, gql, nil, &response); err != nil {
		return Viewer{}, fmt.Errorf("get AniList viewer: %w", err)
	}
	return response.Viewer, nil
}

func (c *Client) ListEntry(ctx context.Context, mediaID int) (ListEntry, bool, error) {
	const gql = `query ($id: Int!) { Media(id: $id, type: ANIME) { mediaListEntry { id status progress } } }`
	var response struct {
		Media struct {
			Entry *ListEntry `json:"mediaListEntry"`
		} `json:"Media"`
	}
	if err := c.do(ctx, gql, map[string]any{"id": mediaID}, &response); err != nil {
		return ListEntry{}, false, fmt.Errorf("get AniList progress for media %d: %w", mediaID, err)
	}
	if response.Media.Entry == nil {
		return ListEntry{}, false, nil
	}
	return *response.Media.Entry, true, nil
}

func (c *Client) SaveProgress(ctx context.Context, mediaID, progress int, status string) (ListEntry, error) {
	const gql = `mutation ($mediaId: Int!, $progress: Int!, $status: MediaListStatus) { SaveMediaListEntry(mediaId: $mediaId, progress: $progress, status: $status) { id status progress } }`
	variables := map[string]any{"mediaId": mediaID, "progress": progress, "status": status}
	var response struct {
		Entry ListEntry `json:"SaveMediaListEntry"`
	}
	if err := c.do(ctx, gql, variables, &response); err != nil {
		return ListEntry{}, fmt.Errorf("save AniList progress for media %d: %w", mediaID, err)
	}
	return response.Entry, nil
}

func (c *Client) List(ctx context.Context, status string) ([]ListItem, error) {
	const gql = `query ($status: MediaListStatus!) { MediaListCollection(type: ANIME, status: $status) { lists { entries { status progress media { ` + mediaFields + ` } } } } }`
	var response struct {
		Collection struct {
			Lists []struct {
				Entries []struct {
					Status   string        `json:"status"`
					Progress int           `json:"progress"`
					Media    mediaResponse `json:"media"`
				} `json:"entries"`
			} `json:"lists"`
		} `json:"MediaListCollection"`
	}
	if err := c.do(ctx, gql, map[string]any{"status": status}, &response); err != nil {
		return nil, fmt.Errorf("list AniList media with status %s: %w", status, err)
	}
	var items []ListItem
	for _, list := range response.Collection.Lists {
		for _, entry := range list.Entries {
			items = append(items, ListItem{Media: entry.Media.toMedia(), Status: entry.Status, Progress: entry.Progress})
		}
	}
	return items, nil
}

func (c *Client) SetStatus(ctx context.Context, mediaID int, status string) error {
	const gql = `mutation ($mediaId: Int!, $status: MediaListStatus!) { SaveMediaListEntry(mediaId: $mediaId, status: $status) { id } }`
	if err := c.do(ctx, gql, map[string]any{"mediaId": mediaID, "status": status}, &struct {
		Entry struct {
			ID int `json:"id"`
		} `json:"SaveMediaListEntry"`
	}{}); err != nil {
		return fmt.Errorf("set AniList status for media %d: %w", mediaID, err)
	}
	return nil
}

type mediaResponse struct {
	ID    int `json:"_id"`
	Title struct {
		Romaji  string `json:"romaji"`
		English string `json:"english"`
		Native  string `json:"native"`
	} `json:"title"`
	Synonyms []string `json:"synonyms"`
	Format   string   `json:"format"`
	Episodes int      `json:"episodes"`
	Cover    struct {
		Large string `json:"large"`
	} `json:"coverImage"`
}

func (m mediaResponse) toMedia() Media {
	title := m.Title.English
	if title == "" {
		title = m.Title.Romaji
	}
	if title == "" {
		title = m.Title.Native
	}
	return Media{
		ID: m.ID, Title: title, English: m.Title.English, Romaji: m.Title.Romaji,
		Native: m.Title.Native, Synonyms: m.Synonyms, Format: m.Format,
		Episodes: m.Episodes, CoverURL: m.Cover.Large,
	}
}

func (c *Client) do(ctx context.Context, query string, variables map[string]any, target any) error {
	body, err := json.Marshal(map[string]any{"query": query, "variables": variables})
	if err != nil {
		return fmt.Errorf("encode GraphQL request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create GraphQL request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.token != nil {
		token, err := c.accessToken()
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("send GraphQL request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode == http.StatusTooManyRequests {
			retryAfter := resp.Header.Get("Retry-After")
			if retryAfter != "" {
				return fmt.Errorf("GraphQL HTTP status %s; retry after %s", resp.Status, retryAfter)
			}
		}
		return fmt.Errorf("GraphQL HTTP status %s", resp.Status)
	}
	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return fmt.Errorf("decode GraphQL response: %w", err)
	}
	if len(envelope.Errors) > 0 {
		return fmt.Errorf("GraphQL: %s", envelope.Errors[0].Message)
	}
	if err := json.Unmarshal(envelope.Data, target); err != nil {
		return fmt.Errorf("decode GraphQL data: %w", err)
	}
	return nil
}
