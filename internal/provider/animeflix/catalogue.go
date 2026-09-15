package animeflix

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"tarragon-anime/internal/provider"
)

const maxSearchPages = 100

// Provider season numbers do not generally match AniList releases (split cours
// are a counterexample). Only individually checked mappings may select one.
var verifiedSeasons = map[int]struct {
	slug, season, name, title string
}{
	182205: {
		slug:   "that-time-i-got-reincarnated-as-a-slime",
		season: "4",
		name:   "That Time I Got Reincarnated as a Slime",
		title:  "That Time I Got Reincarnated as a Slime Season 4",
	},
}

type searchPage struct {
	NextPageAvailable bool `json:"nextpageavailable"`
	AnimeList         []struct {
		Title string `json:"title"`
		ID    string `json:"id"`
	} `json:"animeList"`
}

type infoPage struct {
	InfoX []struct {
		Name string `json:"name"`
	} `json:"infoX"`
	EpisodeTown []struct {
		Season  string `json:"season"`
		Episode string `json:"episode"`
		Name    string `json:"name"`
		EpID    string `json:"epId"`
	} `json:"episodetown"`
}

// Match resolves an AniList title conservatively. Search results are fully
// paged because the API does not carry AniList IDs; the info request confirms
// that a candidate slug is real before it is returned.
func (c *Client) Match(ctx context.Context, mediaID int, aliases []string, translation string) (provider.Anime, error) {
	if mediaID <= 0 {
		return provider.Anime{}, errors.New("AnimeFlix AniList id is invalid")
	}
	if mapping, ok := verifiedSeasons[mediaID]; ok {
		anime := provider.Anime{
			ID:   mapping.slug + "#season=" + mapping.season,
			Name: mapping.title, English: mapping.title, AniListID: mediaID,
		}
		if _, err := c.Episodes(ctx, anime, translation); err != nil {
			return provider.Anime{}, fmt.Errorf("validate AnimeFlix season mapping for AniList %d: %w", mediaID, err)
		}
		return anime, nil
	}

	for _, alias := range uniqueAnimeFlixTitles(aliases) {
		matches := make(map[string]provider.Anime)
		for page := 1; page <= maxSearchPages; page++ {
			var response searchPage
			path := "/search/" + url.PathEscape(strings.TrimSpace(alias)) + "/" + strconv.Itoa(page)
			if err := c.apiJSON(ctx, path, &response); err != nil {
				return provider.Anime{}, fmt.Errorf("search AnimeFlix: %w", err)
			}
			for _, item := range response.AnimeList {
				if strings.TrimSpace(item.ID) == "" || normalizeAnimeFlixTitle(item.Title) != normalizeAnimeFlixTitle(alias) {
					continue
				}
				matches[item.ID] = provider.Anime{ID: item.ID, Name: item.Title, English: item.Title, AniListID: mediaID}
			}
			if !response.NextPageAvailable {
				break
			}
			if page == maxSearchPages {
				return provider.Anime{}, fmt.Errorf("AnimeFlix search exceeded %d pages", maxSearchPages)
			}
		}
		switch len(matches) {
		case 0:
			continue
		case 1:
			for _, anime := range matches {
				var info infoPage
				if err := c.apiJSON(ctx, "/info/"+url.PathEscape(anime.ID), &info); err != nil {
					return provider.Anime{}, fmt.Errorf("validate AnimeFlix match %q: %w", anime.ID, err)
				}
				if _, err := animeFlixEpisodes(anime.ID, info, ""); err != nil {
					return provider.Anime{}, fmt.Errorf("validate AnimeFlix match %q: %w", anime.ID, err)
				}
				return anime, nil
			}
		default:
			return provider.Anime{}, fmt.Errorf("AnimeFlix match is ambiguous for AniList %d", mediaID)
		}
	}
	return provider.Anime{}, fmt.Errorf("no conservative AnimeFlix match for AniList %d", mediaID)
}

func (c *Client) Episodes(ctx context.Context, anime provider.Anime, _ string) ([]provider.Episode, error) {
	id := strings.TrimSpace(anime.ID)
	if id == "" {
		return nil, errors.New("AnimeFlix episodes: show id is required")
	}
	slug, season, expectedName := id, "", ""
	for mediaID, mapping := range verifiedSeasons {
		if id == mapping.slug+"#season="+mapping.season {
			if anime.AniListID != 0 && anime.AniListID != mediaID {
				return nil, errors.New("AnimeFlix season mapping belongs to a different AniList release")
			}
			slug, season, expectedName = mapping.slug, mapping.season, mapping.name
			break
		}
	}
	if strings.Contains(id, "#") && season == "" {
		return nil, errors.New("AnimeFlix season mapping has not been verified")
	}
	var info infoPage
	if err := c.apiJSON(ctx, "/info/"+url.PathEscape(slug), &info); err != nil {
		return nil, fmt.Errorf("list AnimeFlix episodes: %w", err)
	}
	if expectedName != "" && (len(info.InfoX) != 1 || normalizeAnimeFlixTitle(info.InfoX[0].Name) != normalizeAnimeFlixTitle(expectedName)) {
		return nil, errors.New("AnimeFlix season mapping catalogue identity changed")
	}
	return animeFlixEpisodes(id, info, season)
}

func animeFlixEpisodes(id string, info infoPage, season string) ([]provider.Episode, error) {
	if len(info.InfoX) == 0 {
		return nil, errors.New("missing info")
	}
	byNumber := make(map[int]provider.Episode)
	for _, item := range info.EpisodeTown {
		if season != "" && strings.TrimSpace(item.Season) != season {
			continue
		}
		number, err := strconv.Atoi(strings.TrimSpace(item.Episode))
		if err != nil || number <= 0 || strings.TrimSpace(item.EpID) == "" {
			continue
		}
		if season != "" {
			slug, _, _ := strings.Cut(id, "#season=")
			if item.EpID != fmt.Sprintf("%s-%sx%d", slug, season, number) {
				return nil, errors.New("AnimeFlix season mapping episode identity changed")
			}
		}
		ep := provider.Episode{ShowID: id, Number: number, Value: item.EpID}
		if prior, ok := byNumber[number]; ok {
			if prior.Value != ep.Value {
				return nil, fmt.Errorf("AnimeFlix episode number %d has conflicting epIds", number)
			}
			continue
		}
		byNumber[number] = ep
	}
	if len(byNumber) == 0 {
		return nil, fmt.Errorf("AnimeFlix returned no valid episodes for %q", id)
	}
	result := make([]provider.Episode, 0, len(byNumber))
	for _, ep := range byNumber {
		result = append(result, ep)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Number < result[j].Number })
	return result, nil
}

func normalizeAnimeFlixTitle(value string) string {
	var result strings.Builder
	space := false
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			if space && result.Len() > 0 {
				result.WriteByte(' ')
			}
			result.WriteRune(unicode.ToLower(r))
			space = false
			continue
		}
		space = result.Len() > 0
	}
	return strings.TrimSpace(result.String())
}

func uniqueAnimeFlixTitles(values []string) []string {
	seen := make(map[string]bool)
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		normalized := normalizeAnimeFlixTitle(value)
		if normalized == "" {
			continue
		}
		// Search is punctuation-sensitive even when title identity is not.
		// AniList's "Naruto: Shippuden" needs a punctuation-free retry.
		for _, query := range []string{value, normalized} {
			key := strings.ToLower(query)
			if !seen[key] {
				seen[key] = true
				result = append(result, query)
			}
		}
	}
	return result
}
