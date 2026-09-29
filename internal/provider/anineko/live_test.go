//go:build live

package anineko

import (
	"context"
	"net/http"
	"testing"
	"time"

	"tarragon-anime/internal/provider/providertest"
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

func TestLiveStreams(t *testing.T) {
	client := NewClient(&http.Client{Timeout: 20 * time.Second})
	defer client.Close()
	for _, title := range []struct {
		id   int
		name string
	}{
		{21, "One Piece"},
		{154587, "Frieren: Beyond Journey's End"},
	} {
		for _, mode := range []string{"sub", "dub"} {
			t.Run(title.name+"/"+mode, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
				defer cancel()
				show, err := client.Match(ctx, title.id, []string{title.name}, mode)
				if err != nil {
					t.Fatal(err)
				}
				episodes, err := client.Episodes(ctx, show, mode)
				if err != nil || len(episodes) == 0 {
					t.Fatalf("episodes=%d error=%v", len(episodes), err)
				}
				streams, err := client.Streams(ctx, episodes[0], mode, "worst")
				if err != nil || len(streams) == 0 {
					t.Fatalf("streams=%d error=%v", len(streams), err)
				}
				providertest.CheckMedia(t, ctx, streams[0])
			})
		}
	}
}
