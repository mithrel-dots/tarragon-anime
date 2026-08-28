//go:build live

package allanime

import (
	"context"
	"testing"
	"time"
)

func TestLiveVerticalProviderSlice(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	client := NewClient(nil)
	show, err := client.Match(ctx, 154587, []string{"Frieren: Beyond Journey's End", "Sousou no Frieren"}, "sub")
	if err != nil {
		t.Fatal(err)
	}
	episodes, err := client.Episodes(ctx, show, "sub")
	if err != nil {
		t.Fatal(err)
	}
	if len(episodes) == 0 {
		t.Fatal("AllAnime returned no episodes")
	}
	streams, err := client.Streams(ctx, episodes[0], "sub", "best")
	if err != nil {
		t.Fatal(err)
	}
	if len(streams) == 0 || streams[0].URL == "" {
		t.Fatal("AllAnime returned no playable streams")
	}
}
