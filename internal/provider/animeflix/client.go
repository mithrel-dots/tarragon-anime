// Package animeflix resolves AnimeFlix's AnimeSalt catalogue and MyStream HLS.
// Abyss sources are deliberately unsupported: no portable stream is verified.
package animeflix

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"

	"tarragon-anime/internal/provider"
	"tarragon-anime/internal/streamlog"
)

const (
	defaultAPI    = "https://animesalt.backendapia.workers.dev/api/v1"
	defaultOrigin = "https://animesalt.cx"
	userAgent     = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/153.0.0.0 Safari/537.36"
	maxBody       = 4 << 20
)

var myStreamHost = regexp.MustCompile(`^(?:as-cdn[0-9]+\.top|ravok\.buzz)$`)

type Client struct {
	http   *http.Client
	api    string
	origin string

	assetsMu sync.Mutex
	assets   *assetServer
	closed   bool
}

var _ provider.Client = (*Client)(nil)

func NewClient(httpClient *http.Client) *Client {
	return NewClientWithEndpoints(httpClient, defaultAPI, defaultOrigin)
}

func NewClientWithEndpoints(httpClient *http.Client, api, origin string) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if api == "" {
		api = defaultAPI
	}
	if origin == "" {
		origin = defaultOrigin
	}
	return &Client{http: httpClient, api: strings.TrimRight(api, "/"), origin: strings.TrimRight(origin, "/")}
}

func (c *Client) apiJSON(ctx context.Context, path string, out any) error {
	body, _, err := fetch(ctx, c.http, http.MethodGet, c.api+path, c.origin+"/", nil)
	if err != nil {
		return err
	}
	var envelope struct {
		Success bool            `json:"success"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return fmt.Errorf("decode AnimeFlix response: %w", err)
	}
	if !envelope.Success || len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return errors.New("AnimeFlix API returned no successful data")
	}
	return json.Unmarshal(envelope.Data, out)
}

func (c *Client) Streams(ctx context.Context, episode provider.Episode, translation, quality string) ([]provider.Stream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(episode.ShowID) == "" || strings.TrimSpace(episode.Value) == "" || episode.Number <= 0 {
		return nil, errors.New("AnimeFlix streams require a show and episode id")
	}
	translation = strings.ToLower(strings.TrimSpace(translation))
	if translation == "" {
		translation = "sub"
	}
	if translation != "sub" && translation != "dub" {
		return nil, fmt.Errorf("unsupported AnimeFlix translation %q", translation)
	}
	type source struct {
		URL string `json:"url"`
	}
	var sources struct {
		Sub []source `json:"sub"`
		Dub []source `json:"dub"`
	}
	if err := c.apiJSON(ctx, "/server/"+url.PathEscape(episode.Value), &sources); err != nil {
		return nil, fmt.Errorf("load AnimeFlix servers: %w", err)
	}
	// Buckets and labels mix languages. The master playlist, not the API's
	// sub/dub bucket, determines which translation a MyStream source can serve.
	seen := make(map[string]bool)
	var failures []error
	for _, source := range append(sources.Sub, sources.Dub...) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if seen[source.URL] {
			continue
		}
		seen[source.URL] = true
		u, err := url.Parse(source.URL)
		if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || !myStreamHost.MatchString(strings.ToLower(u.Hostname())) {
			finish := streamlog.Start(ctx, fmt.Sprintf("source-%d", len(seen)), "resolve")
			finish("unavailable", "unsupported_source")
			continue
		}
		id := fmt.Sprintf("mystream-%d", len(seen))
		finish := streamlog.Start(ctx, id, "resolve")
		stream, err := c.resolveMyStream(ctx, source.URL, translation, quality)
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		status, reason := streamlog.Outcome(ctx, err)
		if errors.Is(err, errMissingSubtitles) {
			status, reason = "unavailable", "missing_subtitles"
		} else if errors.Is(err, errUnsupportedSource) {
			status, reason = "unavailable", "unsupported_source"
		}
		finish(status, reason)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err == nil {
			stream.Source = id
			return []provider.Stream{stream}, nil
		}
		failures = append(failures, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(failures) > 0 {
		return nil, fmt.Errorf("resolve AnimeFlix MyStream: %w", errors.Join(failures...))
	}
	return nil, errors.New("AnimeFlix has no supported MyStream source; Abyss is unsupported")
}

func fetch(ctx context.Context, client *http.Client, method, endpoint, referer string, form url.Values) ([]byte, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Referer", referer)
	req.Header.Set("Accept", "*/*")
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("HTTP status %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, "", err
	}
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	if len(data) > maxBody {
		return nil, "", errors.New("AnimeFlix response exceeds size limit")
	}
	return data, resp.Request.URL.String(), nil
}
