package aniskip

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
)

const defaultEndpoint = "https://api.aniskip.com/v2"

const (
	Opening = "op"
	Ending  = "ed"
)

type SkipTime struct {
	Type  string
	Start float64
	End   float64
}

type Client struct {
	http     *http.Client
	endpoint string
}

func NewClient(httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{http: httpClient, endpoint: defaultEndpoint}
}

func NewClientWithEndpoint(httpClient *http.Client, endpoint string) *Client {
	client := NewClient(httpClient)
	client.endpoint = endpoint
	return client
}

func (c *Client) SkipTimes(ctx context.Context, malID, episode int, episodeLength float64) ([]SkipTime, error) {
	if malID <= 0 || episode <= 0 || episodeLength <= 0 {
		return nil, nil
	}
	endpoint, err := url.Parse(fmt.Sprintf("%s/skip-times/%d/%d", c.endpoint, malID, episode))
	if err != nil {
		return nil, err
	}
	query := endpoint.Query()
	query.Add("types", Opening)
	query.Add("types", Ending)
	query.Set("episodeLength", strconv.FormatFloat(episodeLength, 'f', 3, 64))
	endpoint.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("AniSkip HTTP status %s", resp.Status)
	}
	var payload struct {
		Results []struct {
			Type string `json:"skipType"`
			Time struct {
				Start float64 `json:"startTime"`
				End   float64 `json:"endTime"`
			} `json:"skipTime"`
		} `json:"results"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload); err != nil {
		return nil, err
	}
	result := make([]SkipTime, 0, len(payload.Results))
	for _, item := range payload.Results {
		if (item.Type != Opening && item.Type != Ending) || item.Time.Start < 0 || item.Time.End <= item.Time.Start {
			continue
		}
		result = append(result, SkipTime{Type: item.Type, Start: item.Time.Start, End: item.Time.End})
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].Start < result[j].Start })
	return result, nil
}
