package allanime

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"tarragon-anime/internal/provider"
)

const (
	defaultAPI       = "https://api.mkissa.net/api"
	defaultOrigin    = "https://mkissa.to"
	defaultClockBase = "https://allanime.day"
	userAgent        = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"
)

const searchQuery = `query($search:SearchInput,$limit:Int,$page:Int,$translationType:VaildTranslationTypeEnumType,$countryOrigin:VaildCountryOriginEnumType){shows(search:$search,limit:$limit,page:$page,translationType:$translationType,countryOrigin:$countryOrigin){edges{_id name englishName aniListId availableEpisodes}}}`
const episodesQuery = `query($showId:String!){show(_id:$showId){_id availableEpisodesDetail}}`
const sourceQuery = `query($showId:String!,$translationType:VaildTranslationTypeEnumType!,$episodeString:String!){episode(showId:$showId translationType:$translationType episodeString:$episodeString){sourceUrls show{_id}}}`
const sourceQueryHash = "436dcab03223760b0ef4a96bef43f640fca6d761b84513eabb7ec13fa9b62a2a"

var ErrCryptoProfileRotated = errors.New("AllAnime crypto profile has rotated; update the plugin")

// Aliases preserve the provider's existing public model names while keeping
// the service dependent on the provider-neutral contract.
type Anime = provider.Anime
type Episode = provider.Episode
type Stream = provider.Stream

type cryptoProfile struct {
	BuildID       string
	Lane          string
	MaskHex       string
	EpochBucketMS int64
	GraceMS       int64
	BootPrefix    string
	BootJoin      string
	BootParts     []string
	KeyGroup      string
	Host          string
}

type cryptoSnapshot struct {
	profile    cryptoProfile
	key        []byte
	epoch      int64
	keyExpiry  time.Time
	generation uint64
}

type cryptoFlight struct {
	done     chan struct{}
	snapshot cryptoSnapshot
	err      error
	retry    bool
}

var currentProfile = cryptoProfile{
	BuildID:       "148",
	Lane:          "k7",
	MaskHex:       "5431adffc5cb1502e4817f2c007b2c3def93b91221aaf808d0b0fea4bf3d30bf",
	EpochBucketMS: 604800000,
	GraceMS:       86400000,
	BootPrefix:    "sXKiyl:",
	BootJoin:      "~",
	BootParts:     []string{"epoch", "host", "buildId", "lane", "group"},
	KeyGroup:      "mkissa",
	Host:          "mkissa.to",
}

type Client struct {
	http      *http.Client
	api       string
	origin    string
	clockBase string
	now       func() time.Time

	mu     sync.Mutex
	state  cryptoSnapshot
	flight *cryptoFlight
}

func NewClient(httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{
		http: httpClient, api: defaultAPI, origin: defaultOrigin,
		clockBase: defaultClockBase, now: time.Now,
		state: cryptoSnapshot{profile: currentProfile},
	}
}

func NewClientWithEndpoints(httpClient *http.Client, api, origin, clockBase string) *Client {
	c := NewClient(httpClient)
	c.api, c.origin, c.clockBase = api, origin, clockBase
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
	if err := c.graphQL(ctx, c.profileSnapshot(), searchQuery, variables, nil, &data); err != nil {
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
	if err := c.graphQL(ctx, c.profileSnapshot(), episodesQuery, map[string]any{"showId": anime.ID}, nil, &data); err != nil {
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

func (c *Client) Streams(ctx context.Context, episode Episode, translation, quality string) ([]Stream, error) {
	snapshot, err := c.cryptoState(ctx)
	if err != nil {
		return nil, err
	}
	token, err := c.aaRequestToken(snapshot)
	if err != nil {
		return nil, fmt.Errorf("build AllAnime source token: %w", err)
	}
	extensions := map[string]any{
		"persistedQuery": map[string]any{"version": 1, "sha256Hash": sourceQueryHash},
		"aaReq":          token,
		"k":              snapshot.profile.Lane,
	}
	var encrypted struct {
		ToBeParsed string `json:"tobeparsed"`
	}
	err = c.graphQL(ctx, snapshot.profile, sourceQuery, map[string]any{
		"showId": episode.ShowID, "translationType": translation, "episodeString": episode.Value,
	}, extensions, &encrypted)
	if err != nil {
		c.clearKey(snapshot)
		return nil, fmt.Errorf("resolve AllAnime episode %d: %w", episode.Number, err)
	}
	plain, err := decryptEnvelope(snapshot.key, encrypted.ToBeParsed)
	if err != nil {
		return nil, fmt.Errorf("decode AllAnime stream: %w", err)
	}
	sources, err := parseSources(plain)
	if err != nil {
		return nil, fmt.Errorf("parse AllAnime streams: %w", err)
	}
	return c.resolveSources(ctx, sources, quality)
}

type source struct {
	URL  string `json:"sourceUrl"`
	Name string `json:"sourceName"`
	Type string `json:"type"`
}

func parseSources(plain []byte) ([]source, error) {
	var payload struct {
		Episode struct {
			SourceURLs []source `json:"sourceUrls"`
		} `json:"episode"`
		Data struct {
			Episode struct {
				SourceURLs []source `json:"sourceUrls"`
			} `json:"episode"`
		} `json:"data"`
	}
	if err := json.Unmarshal(plain, &payload); err != nil {
		return nil, err
	}
	if len(payload.Episode.SourceURLs) > 0 {
		return payload.Episode.SourceURLs, nil
	}
	if len(payload.Data.Episode.SourceURLs) > 0 {
		return payload.Data.Episode.SourceURLs, nil
	}
	return nil, fmt.Errorf("provider returned no source URLs")
}

func (c *Client) resolveSources(ctx context.Context, sources []source, quality string) ([]Stream, error) {
	for _, item := range sources {
		if strings.EqualFold(item.Name, "Yt-mp4") && strings.Contains(item.URL, "tools.fast4speed.rsvp") {
			if validHTTPURL(item.URL) {
				return []Stream{{URL: item.URL, Headers: map[string]string{"Referer": c.origin + "/"}}}, nil
			}
		}
	}
	for _, item := range sources {
		if !strings.HasPrefix(item.URL, "--") {
			continue
		}
		clockURL, err := c.decodeClockURL(item.URL)
		if err != nil {
			continue
		}
		streams, err := c.clockStreams(ctx, clockURL, quality)
		if err == nil && len(streams) > 0 {
			return streams, nil
		}
	}
	return nil, fmt.Errorf("no supported direct or HLS streams found")
}

func (c *Client) decodeClockURL(encoded string) (string, error) {
	raw, err := hex.DecodeString(strings.TrimPrefix(encoded, "--"))
	if err != nil {
		return "", fmt.Errorf("decode clock URL: %w", err)
	}
	for i := range raw {
		raw[i] ^= 0x38
	}
	path := strings.Replace(string(raw), "/clock", "/clock.json", 1)
	base, err := url.Parse(c.clockBase)
	if err != nil {
		return "", err
	}
	reference, err := url.Parse(path)
	if err != nil {
		return "", err
	}
	return base.ResolveReference(reference).String(), nil
}

func (c *Client) clockStreams(ctx context.Context, endpoint, quality string) ([]Stream, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	c.webHeaders(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("clock HTTP status %s", resp.Status)
	}
	var payload struct {
		Links []struct {
			Link       string            `json:"link"`
			HLS        bool              `json:"hls"`
			Resolution string            `json:"resolutionStr"`
			Headers    map[string]string `json:"headers"`
			Subtitles  []struct {
				Source  string `json:"src"`
				Default bool   `json:"default"`
			} `json:"subtitles"`
		} `json:"links"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&payload); err != nil {
		return nil, err
	}
	type candidate struct {
		stream Stream
		score  int
	}
	var candidates []candidate
	for _, link := range payload.Links {
		if !link.HLS || !validHTTPURL(link.Link) {
			continue
		}
		headers := link.Headers
		if headers == nil {
			headers = map[string]string{}
		}
		subtitle := ""
		for _, sub := range link.Subtitles {
			if sub.Default && validHTTPURL(sub.Source) {
				subtitle = sub.Source
				break
			}
			if subtitle == "" && validHTTPURL(sub.Source) {
				subtitle = sub.Source
			}
		}
		candidates = append(candidates, candidate{
			stream: Stream{URL: link.Link, Headers: headers, Subtitle: subtitle},
			score:  resolutionScore(link.Resolution),
		})
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("clock response contained no HLS streams")
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if quality == "worst" {
			return candidates[i].score < candidates[j].score
		}
		return candidates[i].score > candidates[j].score
	})
	result := make([]Stream, 0, len(candidates))
	for _, item := range candidates {
		result = append(result, item.stream)
	}
	return result, nil
}

func resolutionScore(value string) int {
	value = strings.ToLower(value)
	for _, suffix := range []string{"p", "hls"} {
		value = strings.TrimSpace(strings.TrimSuffix(value, suffix))
	}
	n, _ := strconv.Atoi(value)
	return n
}

func (c *Client) graphQL(ctx context.Context, profile cryptoProfile, query string, variables, extensions map[string]any, target any) error {
	payload := map[string]any{"query": query, "variables": variables}
	if extensions != nil {
		payload["extensions"] = extensions
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.api, bytes.NewReader(body))
	if err != nil {
		return err
	}
	c.webHeaders(req)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-build-id", profile.BuildID)
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
		message := envelope.Errors[0].Message
		if strings.Contains(strings.ToLower(message), "crypto") || strings.Contains(message, "unknown_build_id") {
			return fmt.Errorf("%w: %s", ErrCryptoProfileRotated, message)
		}
		return fmt.Errorf("GraphQL: %s", message)
	}
	if err := json.Unmarshal(envelope.Data, target); err != nil {
		return err
	}
	return nil
}

func (c *Client) cryptoState(ctx context.Context) (cryptoSnapshot, error) {
	for {
		c.mu.Lock()
		if len(c.state.key) == 32 && c.now().Before(c.state.keyExpiry) {
			snapshot := c.state.clone()
			c.mu.Unlock()
			return snapshot, nil
		}
		if flight := c.flight; flight != nil {
			c.mu.Unlock()
			select {
			case <-ctx.Done():
				return cryptoSnapshot{}, ctx.Err()
			case <-flight.done:
				if flight.retry {
					continue
				}
				return flight.snapshot.clone(), flight.err
			}
		}
		if err := ctx.Err(); err != nil {
			c.mu.Unlock()
			return cryptoSnapshot{}, err
		}
		flight := &cryptoFlight{done: make(chan struct{})}
		c.flight = flight
		profile := c.state.clone().profile
		c.mu.Unlock()

		snapshot, err := c.loadCryptoState(ctx, profile)
		c.mu.Lock()
		if err == nil {
			snapshot.generation = c.state.generation + 1
			c.state = snapshot.clone()
			flight.snapshot = c.state.clone()
		}
		flight.err = err
		flight.retry = err != nil && ctx.Err() != nil
		c.flight = nil
		close(flight.done)
		c.mu.Unlock()
		return flight.snapshot.clone(), err
	}
}

func (c *Client) loadCryptoState(ctx context.Context, profile cryptoProfile) (cryptoSnapshot, error) {
	key, epoch, expiry, err := c.bootstrapKey(ctx, profile)
	if err == nil {
		return cryptoSnapshot{profile: profile, key: key, epoch: epoch, keyExpiry: expiry}, nil
	}
	lastErr := err
	profiles, refreshErr := c.refreshProfiles(ctx)
	if refreshErr != nil {
		return cryptoSnapshot{}, fmt.Errorf("%w: bootstrap failed: %v; refresh profile: %v", ErrCryptoProfileRotated, lastErr, refreshErr)
	}
	for _, profile := range profiles {
		key, epoch, expiry, err := c.bootstrapKey(ctx, profile)
		if err != nil {
			lastErr = err
			continue
		}
		return cryptoSnapshot{profile: profile, key: key, epoch: epoch, keyExpiry: expiry}, nil
	}
	return cryptoSnapshot{}, fmt.Errorf("%w: bootstrap failed: %v", ErrCryptoProfileRotated, lastErr)
}

func (c *Client) bootstrapKey(ctx context.Context, profile cryptoProfile) ([]byte, int64, time.Time, error) {
	nowMS := c.now().UnixMilli()
	current := nowMS / profile.EpochBucketMS
	adjusted := current
	if nowMS-current*profile.EpochBucketMS < profile.GraceMS && current > 0 {
		adjusted--
	}
	epochs := []int64{adjusted}
	if current != adjusted {
		epochs = append(epochs, current)
	}
	var lastErr error
	for _, epoch := range epochs {
		key, expiry, err := c.bootstrap(ctx, profile, epoch)
		if err == nil {
			return key, epoch, expiry, nil
		}
		lastErr = err
	}
	return nil, 0, time.Time{}, lastErr
}

func (c *Client) bootstrap(ctx context.Context, profile cryptoProfile, epoch int64) ([]byte, time.Time, error) {
	mask, err := hex.DecodeString(profile.MaskHex)
	if err != nil || len(mask) != 32 {
		return nil, time.Time{}, fmt.Errorf("invalid crypto mask")
	}
	inner := hmacSHA256(mask, []byte(profile.BootPrefix+profile.BuildID))
	values := map[string]string{
		"epoch": fmt.Sprint(epoch), "host": profile.Host, "buildId": profile.BuildID,
		"lane": profile.Lane, "group": profile.KeyGroup,
	}
	parts := make([]string, 0, len(profile.BootParts))
	for _, part := range profile.BootParts {
		value, ok := values[part]
		if !ok {
			return nil, time.Time{}, fmt.Errorf("invalid crypto boot part %q", part)
		}
		parts = append(parts, value)
	}
	message := strings.Join(parts, profile.BootJoin)
	signature := hex.EncodeToString(hmacSHA256(inner, []byte(message)))
	endpoint := strings.TrimSuffix(c.api, "/api") + "/client-crypto/v1/bootstrap?buildId=" + url.QueryEscape(profile.BuildID) + "&k=" + url.QueryEscape(profile.Lane)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, time.Time{}, err
	}
	c.webHeaders(req)
	req.Header.Set("x-build-id", profile.BuildID)
	req.Header.Set("x-aa-boot", signature)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, time.Time{}, fmt.Errorf("bootstrap HTTP status %s", resp.Status)
	}
	var payload struct {
		Epoch    int64  `json:"epoch"`
		PartB    string `json:"partB"`
		SwitchAt int64  `json:"switchAt"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload); err != nil {
		return nil, time.Time{}, err
	}
	partB, err := base64.StdEncoding.DecodeString(payload.PartB)
	if err != nil || len(partB) != len(mask) {
		return nil, time.Time{}, fmt.Errorf("invalid bootstrap key material")
	}
	key := make([]byte, len(mask))
	for i := range mask {
		key[i] = mask[i] ^ partB[i]
	}
	expiry := time.UnixMilli(payload.SwitchAt)
	if expiry.Before(c.now()) {
		expiry = c.now().Add(5 * time.Minute)
	}
	return key, expiry, nil
}

func (c *Client) profileSnapshot() cryptoProfile {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state.clone().profile
}

func (snapshot cryptoSnapshot) clone() cryptoSnapshot {
	snapshot.profile.BootParts = append([]string(nil), snapshot.profile.BootParts...)
	snapshot.key = append([]byte(nil), snapshot.key...)
	return snapshot
}

func (c *Client) aaRequestToken(snapshot cryptoSnapshot) (string, error) {
	ts := c.now().UnixMilli() / 300000 * 300000
	ivInput := fmt.Sprintf("%d:%s:%s:%d:%s", snapshot.epoch, snapshot.profile.BuildID, sourceQueryHash, ts, snapshot.profile.Lane)
	digest := sha256.Sum256([]byte(ivInput))
	plain, err := json.Marshal(struct {
		Version int    `json:"v"`
		TS      int64  `json:"ts"`
		Epoch   int64  `json:"epoch"`
		BuildID string `json:"buildId"`
		Hash    string `json:"qh"`
		Lane    string `json:"k"`
	}{1, ts, snapshot.epoch, snapshot.profile.BuildID, sourceQueryHash, snapshot.profile.Lane})
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(snapshot.key)
	if err != nil {
		return "", err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	iv := digest[:aead.NonceSize()]
	envelope := append([]byte{1}, iv...)
	envelope = aead.Seal(envelope, iv, plain, nil)
	return base64.StdEncoding.EncodeToString(envelope), nil
}

func decryptEnvelope(key []byte, value string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(raw) < 1+aead.NonceSize()+aead.Overhead() || raw[0] != 1 {
		return nil, fmt.Errorf("invalid encrypted envelope")
	}
	iv := raw[1 : 1+aead.NonceSize()]
	return aead.Open(nil, iv, raw[1+aead.NonceSize():], nil)
}

func (c *Client) clearKey(snapshot cryptoSnapshot) {
	c.mu.Lock()
	if c.state.generation == snapshot.generation {
		c.state.key, c.state.epoch, c.state.keyExpiry = nil, 0, time.Time{}
	}
	c.mu.Unlock()
}

func (c *Client) webHeaders(req *http.Request) {
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", c.origin)
	req.Header.Set("Referer", c.origin+"/")
	req.Header.Set("User-Agent", userAgent)
}

func hmacSHA256(key, value []byte) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(value)
	return mac.Sum(nil)
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

func validHTTPURL(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != ""
}
