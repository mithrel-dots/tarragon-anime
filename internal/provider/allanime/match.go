package allanime

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

var (
	ordinalSeason = regexp.MustCompile(`\b([0-9]+)(?:st|nd|rd|th)\s+season\b`)
	shortSeason   = regexp.MustCompile(`\bs([0-9]+)\b`)
)

func (c *Client) Match(ctx context.Context, mediaID int, aliases []string, translation string) (Anime, error) {
	queries := uniqueAliases(aliases)
	var titleMatches map[string]Anime
	for _, query := range queries {
		results, err := c.Search(ctx, query, translation)
		if err != nil {
			return Anime{}, err
		}
		for _, candidate := range results {
			if candidate.AniListID == mediaID {
				return candidate, nil
			}
			if matchesAlias(candidate, queries) {
				if titleMatches == nil {
					titleMatches = make(map[string]Anime)
				}
				titleMatches[candidate.ID] = candidate
			}
		}
		if len(titleMatches) > 0 {
			break
		}
	}
	if len(titleMatches) == 1 {
		for _, candidate := range titleMatches {
			return candidate, nil
		}
	}
	if len(titleMatches) > 1 {
		return Anime{}, fmt.Errorf("provider match is ambiguous for AniList %d", mediaID)
	}
	return Anime{}, fmt.Errorf("no conservative provider match for AniList %d", mediaID)
}

func matchesAlias(candidate Anime, aliases []string) bool {
	for _, title := range []string{candidate.Name, candidate.English} {
		normalized := NormalizeTitle(title)
		if normalized == "" {
			continue
		}
		for _, alias := range aliases {
			if normalized == NormalizeTitle(alias) {
				return true
			}
		}
	}
	return false
}

func NormalizeTitle(value string) string {
	value = strings.ToLower(value)
	value = ordinalSeason.ReplaceAllString(value, "season $1")
	value = shortSeason.ReplaceAllString(value, "season $1")
	var words []string
	var word strings.Builder
	flush := func() {
		if word.Len() > 0 {
			words = append(words, word.String())
			word.Reset()
		}
	}
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			word.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return strings.Join(words, " ")
}

func uniqueAliases(aliases []string) []string {
	seen := make(map[string]bool)
	result := make([]string, 0, len(aliases))
	for _, alias := range aliases {
		alias = strings.TrimSpace(alias)
		normalized := NormalizeTitle(alias)
		if normalized == "" || seen[normalized] {
			continue
		}
		seen[normalized] = true
		result = append(result, alias)
	}
	return result
}
