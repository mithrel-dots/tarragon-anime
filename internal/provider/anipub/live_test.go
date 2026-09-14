//go:build live

package anipub

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// Metadata resolution is pure HTTP against the site's own endpoints, so it can
// be exercised for real without a browser. Stream resolution cannot: the
// player encrypts its source response, so it needs a capture and lives in the
// browser live tests instead.
func TestLiveMatchAndEpisodes(t *testing.T) {
	client := NewClient(&http.Client{Timeout: 20 * time.Second})
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	// The aliases a tracker supplies, in the order the service passes them.
	aliases := []string{
		"That Time I Got Reincarnated as a Slime Season 4",
		"Tensei Shitara Slime Datta Ken 4th Season",
	}
	anime, err := client.Match(ctx, 182205, aliases, "sub")
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
	// Episode one is stored beside the list rather than inside it, so its
	// absence is the failure this guards against.
	if episodes[0].Number != 1 {
		t.Fatalf("episodes[0].Number = %d, want the first episode", episodes[0].Number)
	}
	t.Logf("episodes=%d first=%+v last=%+v", len(episodes), episodes[0], episodes[len(episodes)-1])

	if got := watchURL(client.base, episodes[0].ShowID, episodes[0].Value, "sub"); got == "" {
		t.Fatal("watchURL() produced nothing for the first episode")
	}
}
