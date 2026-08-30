package aniskip

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSkipTimes(t *testing.T) {
	requestErrors := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/skip-times/52991/4" || len(r.URL.Query()["types"]) != 2 || r.URL.Query().Get("episodeLength") != "1400.000" {
			requestErrors <- fmt.Errorf("request = %s", r.URL.String())
		} else {
			requestErrors <- nil
		}
		_, _ = w.Write([]byte(`{"results":[{"skipType":"op","interval":{"startTime":0,"endTime":89.5}},{"skipType":"ed","interval":{"startTime":1380,"endTime":1400}},{"skipType":"mixed","interval":{"startTime":1,"endTime":2}},{"skipType":"op","interval":{"startTime":4,"endTime":3}}]}`))
	}))
	defer server.Close()

	times, err := NewClientWithEndpoint(server.Client(), server.URL+"/v2").SkipTimes(context.Background(), 52991, 4, 1400)
	if requestErr := <-requestErrors; requestErr != nil {
		t.Fatal(requestErr)
	}
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
