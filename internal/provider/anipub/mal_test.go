package anipub

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestMALLookupRequiresExactPlayerIdentity(t *testing.T) {
	for _, test := range []struct {
		name, embed string
		status      int
		want        bool
	}{
		{"exact release", "/stream/mal/61967/1/sub", 200, true},
		{"original series", "/stream/mal/34572/1/sub", 200, false},
		{"wrong episode", "/stream/mal/61967/2/sub", 200, false},
		{"wrong translation", "/stream/mal/61967/1/dub", 200, false},
		{"opaque video ID", "/stream/s-2/180105/sub", 200, false},
		{"missing player", "", 200, false},
		{"unavailable", "/stream/mal/61967/1/sub", 404, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/play/61967/1/sub" {
					t.Errorf("unexpected catalogue request: %s", r.URL)
				}
				w.WriteHeader(test.status)
				if test.embed != "" {
					fmt.Fprintf(w, `<iframe src="%s"></iframe>`, test.embed)
				}
			}))
			defer server.Close()
			client := NewClientWithBaseURL(server.Client(), server.URL)
			defer client.Close()
			episode, err := client.LookupMALEpisode(t.Context(), 61967, 1, "sub")
			if !test.want {
				if err == nil || episode != (Episode{}) {
					t.Fatalf("accepted mismatched player: %+v, %v", episode, err)
				}
			} else if err != nil || episode != (Episode{ShowID: "61967", Number: 1, Value: "1"}) {
				t.Fatalf("MAL lookup = %+v, %v", episode, err)
			}
		})
	}
}

func TestMALLookupRejectsInvalidAndCancelledRequests(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
	}))
	defer server.Close()
	client := NewClientWithBaseURL(server.Client(), server.URL)
	defer client.Close()
	for _, test := range []struct {
		mal, number int
		translation string
	}{
		{0, 1, "sub"}, {-1, 1, "sub"}, {61967, 0, "sub"}, {61967, 1, "raw"},
	} {
		if _, err := client.LookupMALEpisode(t.Context(), test.mal, test.number, test.translation); err == nil {
			t.Fatalf("accepted invalid request: %+v", test)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := client.LookupMALEpisode(ctx, 61967, 1, "sub"); err != context.Canceled {
		t.Fatalf("cancelled lookup: %v", err)
	}
	if requests.Load() != 0 {
		t.Fatalf("invalid requests reached the network: %d", requests.Load())
	}
}
