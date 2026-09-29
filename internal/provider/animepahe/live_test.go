//go:build live

package animepahe

import (
	"context"
	"testing"
	"time"

	"tarragon-anime/internal/provider/providertest"
)

// TestLiveAnimePahe exercises the real metadata and player flow. It is opt-in
// because the public origin may be unavailable or protected by Cloudflare.
func TestLiveAnimePahe(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	client := NewClient(nil)
	anime, err := client.Match(ctx, 0, []string{"Frieren: Beyond Journey's End"}, "sub")
	if err != nil {
		t.Fatalf("live AnimePahe match blocked or failed: %v", err)
	}
	episodes, err := client.Episodes(ctx, anime, "sub")
	if err != nil {
		t.Fatalf("live AnimePahe episodes failed: %v", err)
	}
	if len(episodes) == 0 {
		t.Fatal("live AnimePahe returned no episodes")
	}
	streams, err := client.Streams(ctx, episodes[0], "sub", "worst")
	if err != nil || len(streams) == 0 {
		t.Fatalf("live AnimePahe stream blocked or failed: %v", err)
	}
	providertest.CheckMedia(t, ctx, streams[0])
}
