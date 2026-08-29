package aniskip

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSkipTimes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/skip-times/52991/4" || len(r.URL.Query()["types"]) != 2 || r.URL.Query().Get("episodeLength") != "1400.000" {
			t.Fatalf("request = %s", r.URL.String())
		}
		_, _ = w.Write([]byte(`{"results":[{"skipType":"op","skipTime":{"startTime":0,"endTime":89.5}},{"skipType":"ed","skipTime":{"startTime":1380,"endTime":1400}},{"skipType":"mixed","skipTime":{"startTime":1,"endTime":2}},{"skipType":"op","skipTime":{"startTime":4,"endTime":3}}]}`))
	}))
	defer server.Close()

	times, err := NewClientWithEndpoint(server.Client(), server.URL+"/v2").SkipTimes(context.Background(), 52991, 4, 1400)
	if err != nil {
		t.Fatal(err)
	}
	if len(times) != 2 || times[0].Type != Opening || times[0].End != 89.5 || times[1].Type != Ending {
		t.Fatalf("SkipTimes() = %#v", times)
	}
}

func TestSkipTimesMissingEpisodeIsNotAnError(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	got, err := NewClientWithEndpoint(server.Client(), server.URL).SkipTimes(context.Background(), 1, 1, 1)
	if err != nil || got != nil {
		t.Fatalf("SkipTimes() = %#v, %v", got, err)
	}
}
