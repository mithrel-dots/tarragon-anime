//go:build live

package anilist

import (
	"context"
	"testing"
	"time"
)

func TestLiveSearch(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	media, err := NewClient(nil).Search(ctx, "frieren")
	if err != nil {
		t.Fatal(err)
	}
	if len(media) == 0 {
		t.Fatal("AniList returned no search results")
	}
}

func TestLiveGetAiredEpisodes(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	media, err := NewClient(nil).Get(ctx, 182205)
	if err != nil {
		t.Fatal(err)
	}
	if media.ID != 182205 || media.AiredEpisodes < 23 || (media.Episodes > 0 && media.AiredEpisodes > media.Episodes) {
		t.Fatalf("unexpected airing metadata: %#v", media)
	}
	t.Logf("%s: %d aired of %d planned episodes", media.Title, media.AiredEpisodes, media.Episodes)
}
