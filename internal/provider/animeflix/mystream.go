package animeflix

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"tarragon-anime/internal/provider"
)

var (
	videoPathRE = regexp.MustCompile(`^/video/([a-fA-F0-9]{32})/?$`)
	subtitleRE  = regexp.MustCompile(`\bplayerjsSubtitle\s*=\s*("(?:\\.|[^"\\])*")`)
	trackRE     = regexp.MustCompile(`\[([^\]]+)\]([^\[]+)`)
	attributeRE = regexp.MustCompile(`([A-Z0-9-]+)=("[^"]*"|[^,]*)`)
	cueRE       = regexp.MustCompile(`(?m)^(?:([0-9]+):)?([0-9]{1,2}):([0-9]{2})[.,]([0-9]{1,3})[ \t]+-->[ \t]+(?:([0-9]+):)?([0-9]{1,2}):([0-9]{2})[.,]([0-9]{1,3})([^\n]*)$`)
)

func (c *Client) resolveMyStream(ctx context.Context, embedURL, translation, quality string) (provider.Stream, error) {
	// A jar belongs to one resolution, never to the caller's shared HTTP client:
	// active playback and next-episode prefetch can establish different sessions.
	client := *c.http
	client.Jar, _ = cookiejar.New(nil)
	page, pageURL, err := fetch(ctx, &client, http.MethodGet, embedURL, c.origin+"/", nil)
	if err != nil {
		return provider.Stream{}, fmt.Errorf("load MyStream player: %w", err)
	}
	u, err := url.Parse(pageURL)
	if err != nil {
		return provider.Stream{}, err
	}
	path := videoPathRE.FindStringSubmatch(u.Path)
	if path == nil {
		return provider.Stream{}, errors.New("unsupported MyStream player path")
	}
	endpoint := u.ResolveReference(&url.URL{Path: "/player/index.php", RawQuery: url.Values{"data": {path[1]}, "do": {"getVideo"}}.Encode()})
	body, _, err := fetch(ctx, &client, http.MethodPost, endpoint.String(), pageURL, url.Values{"hash": {path[1]}, "r": {c.origin + "/"}})
	if err != nil {
		return provider.Stream{}, fmt.Errorf("load MyStream source: %w", err)
	}
	var source struct {
		HLS         bool   `json:"hls"`
		VideoSource string `json:"videoSource"`
	}
	if err := json.Unmarshal(body, &source); err != nil {
		return provider.Stream{}, fmt.Errorf("decode MyStream source: %w", err)
	}
	masterURL, err := mediaURL(pageURL, source.VideoSource)
	if err != nil || !source.HLS {
		return provider.Stream{}, errors.New("MyStream returned no HLS source")
	}
	// Media is portable without player cookies. Verify the master that way and
	// never forward session cookies to arbitrary CDN/subtitle hosts through mpv.
	client.Jar = nil
	master, masterURL, err := fetch(ctx, &client, http.MethodGet, masterURL, pageURL, nil)
	if err != nil {
		return provider.Stream{}, fmt.Errorf("load MyStream master: %w", err)
	}
	language := "jpn"
	if translation == "dub" {
		language = "eng"
	}
	master, err = filterMaster(masterURL, string(master), language, quality)
	if err != nil {
		return provider.Stream{}, err
	}
	var subtitle []byte
	if translation == "sub" {
		subtitleURL, err := englishSubtitle(pageURL, string(page))
		if err != nil {
			return provider.Stream{}, err
		}
		subtitle, _, err = fetch(ctx, &client, http.MethodGet, subtitleURL, pageURL, nil)
		if err != nil {
			return provider.Stream{}, fmt.Errorf("load MyStream English subtitles: %w", err)
		}
		subtitle, err = normalizeSubtitle(string(subtitle))
		if err != nil {
			return provider.Stream{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return provider.Stream{}, err
	}
	localMaster, localSubtitle, err := c.publish(master, subtitle)
	if err != nil {
		return provider.Stream{}, err
	}
	return provider.Stream{
		URL: localMaster, Subtitle: localSubtitle,
		Headers: map[string]string{"Referer": pageURL, "User-Agent": userAgent},
	}, nil
}

func mediaURL(base, raw string) (string, error) {
	if strings.TrimSpace(raw) == "" || strings.ContainsAny(raw, "\r\n\"") {
		return "", errors.New("empty or invalid media URL")
	}
	b, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", err
	}
	u = b.ResolveReference(u)
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return "", errors.New("unsupported media URL")
	}
	return u.String(), nil
}

func englishSubtitle(base, page string) (string, error) {
	match := subtitleRE.FindStringSubmatch(page)
	if match != nil {
		var tracks string
		if err := json.Unmarshal([]byte(match[1]), &tracks); err != nil {
			return "", fmt.Errorf("decode MyStream subtitles: %w", err)
		}
		for _, track := range trackRE.FindAllStringSubmatch(tracks, -1) {
			if strings.EqualFold(strings.TrimSpace(track[1]), "English") {
				return mediaURL(base, strings.TrimSuffix(strings.TrimSpace(track[2]), ","))
			}
		}
	}
	return "", errors.New("MyStream has no verified full English subtitle track")
}

func normalizeSubtitle(raw string) ([]byte, error) {
	raw = strings.TrimSpace(strings.TrimPrefix(strings.ReplaceAll(raw, "\r\n", "\n"), "\ufeff"))
	if strings.HasPrefix(strings.ToLower(raw), "<") || !cueRE.MatchString(raw) {
		return nil, errors.New("MyStream subtitle response has no valid text cues")
	}
	valid := true
	raw = cueRE.ReplaceAllStringFunc(raw, func(line string) string {
		parts := cueRE.FindStringSubmatch(line)
		var times []string
		for _, start := range []int{1, 5} {
			hours, _ := strconv.Atoi(parts[start])
			minutes, _ := strconv.Atoi(parts[start+1])
			seconds, _ := strconv.Atoi(parts[start+2])
			if minutes >= 60 || seconds >= 60 {
				valid = false
			}
			fraction := parts[start+3] + strings.Repeat("0", 3-len(parts[start+3]))
			times = append(times, fmt.Sprintf("%02d:%02d:%02d.%s", hours, minutes, seconds, fraction))
		}
		return times[0] + " --> " + times[1] + parts[9]
	})
	if !valid {
		return nil, errors.New("MyStream subtitle has invalid cue times")
	}
	// MyStream serves headerless SRT-like text from .jpg URLs, sometimes using
	// centiseconds. Normalize it to WebVTT instead of relying on MIME sniffing.
	if !strings.HasPrefix(raw, "WEBVTT") {
		raw = "WEBVTT\n\n" + raw
	}
	return []byte(raw + "\n"), nil
}

func hlsAttributes(line string) map[string]string {
	result := make(map[string]string)
	for _, match := range attributeRE.FindAllStringSubmatch(line, -1) {
		result[match[1]] = match[2]
	}
	return result
}

func attributeValue(attrs map[string]string, name string) string {
	return strings.Trim(attrs[name], "\"")
}

func encodeAttributes(attrs map[string]string) string {
	keys := make([]string, 0, len(attrs))
	for key := range attrs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+attrs[key])
	}
	return strings.Join(parts, ",")
}

func filterMaster(base, body, language, quality string) ([]byte, error) {
	if !strings.HasPrefix(strings.TrimSpace(body), "#EXTM3U") {
		return nil, errors.New("MyStream response is not an HLS playlist")
	}
	type variant struct {
		attrs     map[string]string
		url       string
		height    int
		bandwidth int64
	}
	audio := make(map[string]map[string]string)
	var variants []variant
	var pending map[string]string
	var globals []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "#EXT-X-SESSION-KEY:"), strings.HasPrefix(line, "#EXT-X-DEFINE:"):
			return nil, errors.New("MyStream master requires unsupported session keys or variables")
		case strings.HasPrefix(line, "#EXT-X-VERSION:"), line == "#EXT-X-INDEPENDENT-SEGMENTS":
			globals = append(globals, line)
		case strings.HasPrefix(line, "#EXT-X-MEDIA:"):
			attrs := hlsAttributes(line)
			lang := strings.ToLower(attributeValue(attrs, "LANGUAGE"))
			name := strings.ToLower(attributeValue(attrs, "NAME"))
			matches := (language == "jpn" && (lang == "jpn" || lang == "ja" || (lang == "" && name == "japanese"))) ||
				(language == "eng" && (lang == "eng" || lang == "en" || (lang == "" && name == "english")))
			group := attributeValue(attrs, "GROUP-ID")
			if attributeValue(attrs, "TYPE") != "AUDIO" || !matches || group == "" || audio[group] != nil {
				continue
			}
			uri, err := mediaURL(base, attributeValue(attrs, "URI"))
			if err != nil {
				return nil, fmt.Errorf("invalid MyStream audio URI: %w", err)
			}
			attrs["URI"] = "\"" + uri + "\""
			attrs["DEFAULT"], attrs["AUTOSELECT"] = "YES", "YES"
			audio[group] = attrs
		case strings.HasPrefix(line, "#EXT-X-STREAM-INF:"):
			pending = hlsAttributes(line)
		case line != "" && !strings.HasPrefix(line, "#") && pending != nil:
			uri, err := mediaURL(base, line)
			if err != nil {
				return nil, fmt.Errorf("invalid MyStream video URI: %w", err)
			}
			_, height, _ := strings.Cut(attributeValue(pending, "RESOLUTION"), "x")
			h, _ := strconv.Atoi(height)
			bandwidth, _ := strconv.ParseInt(pending["BANDWIDTH"], 10, 64)
			variants = append(variants, variant{pending, uri, h, bandwidth})
			pending = nil
		}
	}
	var candidates []variant
	for _, v := range variants {
		if audio[attributeValue(v.attrs, "AUDIO")] != nil && v.attrs["VIDEO"] == "" {
			candidates = append(candidates, v)
		}
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("MyStream has no video with verified %s audio", language)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].height != candidates[j].height {
			return candidates[i].height < candidates[j].height
		}
		return candidates[i].bandwidth < candidates[j].bandwidth
	})
	chosen := candidates[len(candidates)-1]
	quality = strings.ToLower(strings.TrimSpace(quality))
	if quality == "worst" {
		chosen = candidates[0]
	} else if height, err := strconv.Atoi(strings.TrimSuffix(quality, "p")); err == nil && height > 0 {
		chosen = candidates[0]
		for _, v := range candidates {
			if v.height <= height {
				chosen = v
			}
		}
	}
	delete(chosen.attrs, "SUBTITLES")
	lines := append([]string{"#EXTM3U"}, globals...)
	lines = append(lines, "#EXT-X-MEDIA:"+encodeAttributes(audio[attributeValue(chosen.attrs, "AUDIO")]),
		"#EXT-X-STREAM-INF:"+encodeAttributes(chosen.attrs), chosen.url, "")
	return []byte(strings.Join(lines, "\n")), nil
}
