package anineko

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var (
	bibiembMasterRE = regexp.MustCompile(`(?i)const\s+src\s*=\s*["'](https?://[^"']+/master\.m3u8[^"']*)["']`)
	vibeMasterRE    = regexp.MustCompile(`(?i)const\s+src\s*=\s*["'](https?://[^"']+/public/stream/[^"']+/master\.m3u8[^"']*)["']`)
	streamInfRE     = regexp.MustCompile(`(?m)^#EXT-X-STREAM-INF:([^\r\n]*)\r?\n([^\r\n#]+)`)
	nameRE          = regexp.MustCompile(`(?i)(?:^|,)NAME="([^"]+)"`)
	resolutionRE    = regexp.MustCompile(`(?i)(?:^|,)RESOLUTION=(\d+)x(\d+)`)
	bandwidthRE     = regexp.MustCompile(`(?i)(?:^|,)BANDWIDTH=(\d+)`)
)

type variant struct {
	name      string
	URL       string
	height    int
	bandwidth int
	order     int
}

func (c *Client) resolveBibiemb(ctx context.Context, embedURL, quality string) (Stream, error) {
	page, err := c.fetchString(ctx, embedURL, c.base+"/")
	if err != nil {
		return Stream{}, err
	}
	match := bibiembMasterRE.FindStringSubmatch(page)
	if len(match) < 2 {
		return Stream{}, fmt.Errorf("bibiemb master playlist not found")
	}
	masterURL := strings.TrimSpace(match[1])
	playlist, err := c.fetchString(ctx, masterURL, embedURL)
	if err != nil {
		return Stream{}, err
	}
	streamURL := masterURL
	if variants := parseVariants(masterURL, playlist); len(variants) > 0 {
		streamURL = chooseVariant(variants, quality).URL
	}
	return Stream{
		URL:      streamURL,
		Headers:  map[string]string{"Referer": embedURL},
		Subtitle: resolveEmbedResource(embedURL, resolveSubtitle(embedURL, page)),
	}, nil
}

func (c *Client) resolveVibeplayer(ctx context.Context, embedURL string) (Stream, error) {
	page, err := c.fetchString(ctx, embedURL, c.base+"/")
	if err != nil {
		return Stream{}, err
	}
	match := vibeMasterRE.FindStringSubmatch(page)
	if len(match) < 2 {
		return Stream{}, fmt.Errorf("vibeplayer master playlist not found")
	}
	masterURL := strings.TrimSpace(match[1])
	proxy, err := c.vibeProxy(ctx)
	if err != nil {
		return Stream{}, err
	}
	proxyURL, err := proxy.register(ctx, masterURL, embedURL)
	if err != nil {
		return Stream{}, err
	}
	return Stream{
		URL:      proxyURL,
		Headers:  map[string]string{"Referer": embedURL},
		Subtitle: resolveEmbedResource(embedURL, resolveSubtitle(embedURL, page)),
	}, nil
}

func parseVariants(masterURL, playlist string) []variant {
	matches := streamInfRE.FindAllStringSubmatch(playlist, -1)
	variants := make([]variant, 0, len(matches))
	for index, match := range matches {
		if len(match) < 3 {
			continue
		}
		attributes := match[1]
		entry := strings.TrimSpace(match[2])
		if entry == "" {
			continue
		}
		item := variant{URL: resolvePlaylistURL(masterURL, entry), order: index}
		if value := nameRE.FindStringSubmatch(attributes); len(value) >= 2 {
			item.name = strings.TrimSpace(value[1])
		}
		if value := resolutionRE.FindStringSubmatch(attributes); len(value) >= 3 {
			item.height, _ = strconv.Atoi(value[2])
		}
		if value := bandwidthRE.FindStringSubmatch(attributes); len(value) >= 2 {
			item.bandwidth, _ = strconv.Atoi(value[1])
		}
		if item.height == 0 {
			item.height = qualityNumber(item.name)
		}
		variants = append(variants, item)
	}
	return variants
}

func chooseVariant(variants []variant, quality string) variant {
	if len(variants) == 0 {
		return variant{}
	}
	ordered := append([]variant(nil), variants...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].height != ordered[j].height {
			return ordered[i].height > ordered[j].height
		}
		if ordered[i].bandwidth != ordered[j].bandwidth {
			return ordered[i].bandwidth > ordered[j].bandwidth
		}
		return ordered[i].order < ordered[j].order
	})
	if strings.EqualFold(strings.TrimSpace(quality), "worst") {
		return ordered[len(ordered)-1]
	}
	return ordered[0]
}

func qualityNumber(name string) int {
	for _, part := range strings.FieldsFunc(strings.ToLower(name), func(r rune) bool {
		return r < '0' || r > '9'
	}) {
		if number, err := strconv.Atoi(part); err == nil {
			return number
		}
	}
	return 0
}

func resolvePlaylistURL(rawBase, entry string) string {
	base, err := url.Parse(rawBase)
	if err != nil {
		return entry
	}
	reference, err := url.Parse(strings.TrimSpace(entry))
	if err != nil {
		return entry
	}
	return base.ResolveReference(reference).String()
}

func resolveEmbedResource(embedURL, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	base, err := url.Parse(embedURL)
	if err != nil {
		return raw
	}
	reference, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	return base.ResolveReference(reference).String()
}
