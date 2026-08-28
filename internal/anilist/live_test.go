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
