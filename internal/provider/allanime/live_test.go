//go:build live

package allanime

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestLiveMetadataSlice covers the operations that are still resolved natively
// against the provider's GraphQL API. Stream resolution needs a browser and is
// covered by browser_live_test.go.
func TestLiveMetadataSlice(t *testing.T) {
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
}

// TestLiveStreamsNeedABrowser records the deliberate consequence of retiring
// the native crypto path: without a browser session there is no stream.
func TestLiveStreamsNeedABrowser(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	client := NewClient(nil)
	_, err := client.Streams(ctx, Episode{ShowID: "ReHMC7TQnch3C6z8j", Number: 1, Value: "1"}, "sub", "best")
	if !errors.Is(err, ErrBrowserRequired) {
		t.Fatalf("Streams() error = %v, want ErrBrowserRequired", err)
	}
}
