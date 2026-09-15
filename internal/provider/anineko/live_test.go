//go:build live

package anineko

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func TestLiveMatchAndEpisodes(t *testing.T) {
	client := NewClient(&http.Client{Timeout: 20 * time.Second})
	defer client.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	anime, err := client.Match(ctx, 21, []string{"One Piece"}, "sub")
	if err != nil {
		t.Fatalf("Match() error = %v", err)
	}
	t.Logf("matched id=%s name=%q", anime.ID, anime.Name)

	episodes, err := client.Episodes(ctx, anime, "sub")
	if err != nil {
		t.Fatalf("Episodes() error = %v", err)
	}
	if len(episodes) == 0 {
		t.Fatal("Episodes() returned nothing")
	}
	if episodes[0].Number != 1 {
		t.Fatalf("episodes[0].Number = %d, want the first episode", episodes[0].Number)
	}
	t.Logf("episodes=%d first=%+v last=%+v", len(episodes), episodes[0], episodes[len(episodes)-1])
}
