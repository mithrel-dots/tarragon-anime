package anipub

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var (
	resolutionRE = regexp.MustCompile(`(?:^|,)RESOLUTION=\d+x(\d+)`)
	uriRE        = regexp.MustCompile(`\bURI="([^"]+)"`)
)

type rendition struct {
	height int
	url    string
}

// Select a muxed rendition, preserving the user's quality preference. Separate
// audio renditions need a different master rewrite, rather than silently
// dropping the audio by handing over a video-only playlist.
func selectPlaylist(base, body, quality string) (string, error) {
	if !strings.HasPrefix(strings.TrimSpace(body), "#EXTM3U") {
		return "", errors.New("AniPub response is not an HLS playlist")
	}
	var variants []rendition
	var pending bool
	var height int
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "#EXT-X-MEDIA:"), strings.HasPrefix(line, "#EXT-X-SESSION-KEY:"), strings.HasPrefix(line, "#EXT-X-DEFINE:"):
			return "", errors.New("AniPub playlist uses unsupported alternate tracks or session parameters")
		case strings.HasPrefix(line, "#EXT-X-STREAM-INF:"):
			pending, height = true, 0
			if match := resolutionRE.FindStringSubmatch(strings.TrimPrefix(line, "#EXT-X-STREAM-INF:")); match != nil {
				height, _ = strconv.Atoi(match[1])
			}
		case pending && line != "" && !strings.HasPrefix(line, "#"):
			resolved, err := resourceURL(base, line)
			if err != nil {
				return "", err
			}
			variants = append(variants, rendition{height, resolved})
			pending = false
		}
	}
	if pending {
		return "", errors.New("AniPub master has a missing rendition URL")
	}
	if len(variants) == 0 {
		return "", nil // Already a media playlist.
	}
	sort.SliceStable(variants, func(i, j int) bool { return variants[i].height > variants[j].height })
	quality = strings.ToLower(strings.TrimSpace(quality))
	if quality == "worst" {
		return variants[len(variants)-1].url, nil
	}
	if wanted, err := strconv.Atoi(strings.TrimSuffix(quality, "p")); err == nil && wanted > 0 {
		for _, variant := range variants {
			if variant.height <= wanted {
				return variant.url, nil
			}
		}
		return variants[len(variants)-1].url, nil
	}
	return variants[0].url, nil
}

func (c *Client) prepareStream(ctx context.Context, stream Stream, quality string, codec *sourceCipher) (Stream, error) {
	body, base, err := c.resource(ctx, stream.URL, stream.Headers)
	if err != nil {
		return Stream{}, err
	}
	selected, err := selectPlaylist(base, string(body), quality)
	if err != nil {
		return Stream{}, err
	}
	if selected != "" {
		body, base, err = c.resource(ctx, selected, stream.Headers)
		if err != nil {
			return Stream{}, err
		}
	}
	playlist, resources, err := rewriteMediaPlaylist(base, string(body), codec)
	if err != nil {
		return Stream{}, err
	}
	stream.URL, err = c.publish(ctx, playlist, resources, stream.Headers)
	return stream, err
}

// Local segment names carry their actual media type. MegaPlay's CDN serves TS
// bytes from .jpg/.html/.js URLs; FFmpeg rejects those extensions even though
// Chromium accepts them. Only resources named by this VOD playlist are proxied.
func rewriteMediaPlaylist(base, body string, codec *sourceCipher) (string, map[string]string, error) {
	if !strings.HasPrefix(strings.TrimSpace(body), "#EXTM3U") || !strings.Contains(body, "#EXT-X-ENDLIST") {
		return "", nil, errors.New("AniPub source is not a complete VOD playlist")
	}
	resources := make(map[string]string)
	add := func(raw, extension string) (string, error) {
		resolved, err := resourceURL(base, raw)
		if err != nil {
			return "", err
		}
		u, _ := url.Parse(resolved)
		if strings.HasPrefix(u.Path, "/segment/") {
			plain, err := codec.decrypt(strings.TrimPrefix(u.Path, "/segment/"))
			if err != nil {
				return "", err
			}
			resolved, err = resourceURL(base, string(plain))
			if err != nil {
				return "", err
			}
		}
		name := fmt.Sprintf("%d.%s", len(resources), extension)
		resources[name] = resolved
		return name, nil
	}
	lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	segmentExtension := "ts"
	if strings.Contains(body, "#EXT-X-MAP:") {
		segmentExtension = "m4s"
	}
	segments := 0
	pending := false
	for i, line := range lines {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "#EXT-X-STREAM-INF:"), strings.HasPrefix(line, "#EXT-X-DEFINE:"), strings.HasPrefix(line, "#EXT-X-MEDIA:"), strings.HasPrefix(line, "#EXT-X-SESSION-KEY:"):
			return "", nil, errors.New("unsupported AniPub media playlist")
		case strings.HasPrefix(line, "#EXTINF:"):
			pending = true
		case strings.HasPrefix(line, "#EXT-X-MAP:"), strings.HasPrefix(line, "#EXT-X-KEY:"):
			extension := "key"
			if strings.HasPrefix(line, "#EXT-X-MAP:") {
				extension = "mp4"
			}
			if match := uriRE.FindStringSubmatch(line); match != nil {
				name, err := add(match[1], extension)
				if err != nil {
					return "", nil, err
				}
				lines[i] = strings.Replace(line, match[0], `URI="`+name+`"`, 1)
			}
		case line != "" && !strings.HasPrefix(line, "#"):
			if !pending {
				return "", nil, errors.New("AniPub segment has no duration")
			}
			name, err := add(line, segmentExtension)
			if err != nil {
				return "", nil, err
			}
			lines[i] = name
			segments++
			pending = false
		}
	}
	if pending || segments == 0 {
		return "", nil, errors.New("AniPub playlist has no complete media segments")
	}
	return strings.Join(lines, "\n"), resources, nil
}
