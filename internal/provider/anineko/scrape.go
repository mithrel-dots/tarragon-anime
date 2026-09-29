package anineko

import (
	"html"
	"net/url"
	"regexp"
	"strings"
)

var (
	dataVideoRE      = regexp.MustCompile(`(?i)data-video\s*=\s*["']([^"']+)["']`)
	dataLanguageRE   = regexp.MustCompile(`(?i)data-id\s*=\s*["'](hsub|sub|dub)["']`)
	subtitleRE       = regexp.MustCompile(`(?i)[?&](?:sub|caption_1|c1_file)=([^&"']+)`)
	embedSubtitleRE  = regexp.MustCompile(`(?i)const\s+subtitle\s*=\s*["']([^"']+)["']`)
	embedTrackFileRE = regexp.MustCompile(`(?i)file\s*:\s*["']([^"']+\.(?:vtt|ass|srt))["']`)
)

// extractLangEmbedURLs retains the order in the page because it is also the
// site's server preference order. Duplicate embeds are removed per language.
func extractLangEmbedURLs(page string) map[string][]string {
	groups := map[string][]string{"hsub": {}, "sub": {}, "dub": {}}
	markers := dataLanguageRE.FindAllStringSubmatchIndex(page, -1)
	for index, marker := range markers {
		if len(marker) < 4 {
			continue
		}
		language := strings.ToLower(page[marker[2]:marker[3]])
		start := marker[1]
		end := len(page)
		if index+1 < len(markers) {
			end = markers[index+1][0]
		}
		seen := map[string]bool{}
		for _, match := range dataVideoRE.FindAllStringSubmatch(page[start:end], -1) {
			if len(match) < 2 {
				continue
			}
			embed := strings.TrimSpace(html.UnescapeString(match[1]))
			if embed != "" && !seen[embed] {
				seen[embed] = true
				groups[language] = append(groups[language], embed)
			}
		}
	}
	return groups
}

func embedURLsForMode(groups map[string][]string, translation string) []string {
	if strings.EqualFold(strings.TrimSpace(translation), "dub") {
		return append([]string(nil), groups["dub"]...)
	}
	// Prefer soft subs because they can be turned off or styled in mpv. If a
	// site only exposes hard subs, falling back keeps the provider useful.
	return append(append([]string(nil), groups["sub"]...), groups["hsub"]...)
}

func normalizeTranslation(value string) string {
	if strings.EqualFold(strings.TrimSpace(value), "dub") {
		return "dub"
	}
	return "sub"
}

func resolveEmbedHost(raw string) string {
	host := strings.ToLower(hostFromURL(raw))
	switch {
	case strings.Contains(host, "bibiemb."):
		return "bibiemb"
	case strings.Contains(host, "vibeplayer."), strings.Contains(host, "vivibebe."):
		return "vibeplayer"
	case strings.HasPrefix(host, "vibe") && strings.Contains(host, "be."):
		return "vibeplayer"
	default:
		return ""
	}
}

func hostFromURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return parsed.Host
}

func subtitleFromEmbedURL(embedURL string) string {
	parsed, err := url.Parse(embedURL)
	if err == nil {
		for _, key := range []string{"sub", "caption_1", "c1_file"} {
			if value := strings.TrimSpace(parsed.Query().Get(key)); value != "" {
				return value
			}
		}
	}
	match := subtitleRE.FindStringSubmatch(embedURL)
	if len(match) < 2 {
		return ""
	}
	decoded, err := url.QueryUnescape(match[1])
	if err != nil {
		return match[1]
	}
	return decoded
}

func subtitleFromEmbedHTML(page string) string {
	if match := embedSubtitleRE.FindStringSubmatch(page); len(match) >= 2 {
		if value := strings.TrimSpace(html.UnescapeString(match[1])); value != "" {
			return value
		}
	}
	if match := embedTrackFileRE.FindStringSubmatch(page); len(match) >= 2 {
		return strings.TrimSpace(html.UnescapeString(match[1]))
	}
	return ""
}

func resolveSubtitle(embedURL, page string) string {
	if value := subtitleFromEmbedURL(embedURL); value != "" {
		return value
	}
	return subtitleFromEmbedHTML(page)
}
