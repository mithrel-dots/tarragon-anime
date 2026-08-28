package anime

import (
	"context"
	"testing"

	"tarragon-anime/internal/anilist"
	"tarragon-anime/internal/mpv"
	"tarragon-anime/internal/provider/allanime"
)

type cachingAniList struct {
	getCalls int
}

func (c *cachingAniList) Search(context.Context, string) ([]anilist.Media, error) {
	return []anilist.Media{{ID: 154587, Title: "Frieren", English: "Frieren", Episodes: 28, CoverURL: "https://image.test/cover.jpg"}}, nil
}

func (c *cachingAniList) Get(context.Context, int) (anilist.Media, error) {
	c.getCalls++
	return anilist.Media{ID: 154587, Title: "Frieren", English: "Frieren", Episodes: 28, CoverURL: "https://image.test/cover.jpg"}, nil
}

type cachingProvider struct{}

func (cachingProvider) Match(_ context.Context, id int, _ []string, _ string) (allanime.Anime, error) {
	return allanime.Anime{ID: "show", Name: "Frieren", AniListID: id}, nil
}

func (cachingProvider) Episodes(context.Context, allanime.Anime, string) ([]allanime.Episode, error) {
	return []allanime.Episode{{ShowID: "show", Number: 1, Value: "1"}}, nil
}

func (cachingProvider) Streams(context.Context, allanime.Episode, string, string) ([]allanime.Stream, error) {
	return nil, nil
}

type unusedPlayer struct{}

func (unusedPlayer) Play(context.Context, mpv.Stream, string, float64) (mpv.SessionController, error) {
	return nil, nil
}

type fakePreviewCache struct{}

func (fakePreviewCache) Get(context.Context, int, string) (string, error) {
	return "/tmp/tarragon-anime-cover.jpg", nil
}

func TestEpisodesUsesMediaCachedBySearch(t *testing.T) {
	client := &cachingAniList{}
	service := NewService(client, cachingProvider{}, unusedPlayer{}, nil, nil, DefaultConfig(), nil)
	if _, err := service.Search(t.Context(), "frieren"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Episodes(t.Context(), 154587); err != nil {
		t.Fatal(err)
	}
	if client.getCalls != 0 {
		t.Fatalf("AniList Get() calls = %d, want 0", client.getCalls)
	}
}

func TestSearchUsesLocalPreviewPath(t *testing.T) {
	client := &cachingAniList{}
	service := NewService(client, cachingProvider{}, unusedPlayer{}, nil, fakePreviewCache{}, DefaultConfig(), nil)
	results, err := service.Search(t.Context(), "frieren")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].CoverURL != "/tmp/tarragon-anime-cover.jpg" {
		t.Fatalf("Search() = %#v", results)
	}
}
