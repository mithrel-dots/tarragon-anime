package animeflix

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

func TestPublishServesImmutableAssets(t *testing.T) {
	c := NewClient(nil)
	t.Cleanup(c.Close)
	master := []byte("#EXTM3U\nhttps://origin.example/video.m3u8\n")
	subtitle := []byte("WEBVTT\n\n00:00.000 --> 00:01.000\nHello\n")
	masterURL, subtitleURL, err := c.publish(master, subtitle)
	if err != nil {
		t.Fatal(err)
	}
	master[0], subtitle[0] = 'x', 'x'
	for _, test := range []struct {
		url  string
		want string
		mime string
	}{
		{masterURL, "#EXTM3U\nhttps://origin.example/video.m3u8\n", "application/vnd.apple.mpegurl"},
		{subtitleURL, "WEBVTT\n\n00:00.000 --> 00:01.000\nHello\n", "text/vtt"},
	} {
		response, getErr := http.Get(test.url)
		if getErr != nil {
			t.Fatal(getErr)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != test.mime || response.Header.Get("Cache-Control") != "no-cache" || string(body) != test.want {
			t.Fatalf("GET %s: status=%d mime=%q cache=%q body=%q", test.url, response.StatusCode, response.Header.Get("Content-Type"), response.Header.Get("Cache-Control"), body)
		}
	}

	head, err := http.Head(masterURL)
	if err != nil {
		t.Fatal(err)
	}
	headBody, _ := io.ReadAll(head.Body)
	head.Body.Close()
	if head.StatusCode != http.StatusOK || len(headBody) != 0 {
		t.Fatalf("HEAD: status=%d body=%q", head.StatusCode, headBody)
	}
	c.Close()
}

func TestAssetsRejectInvalidRequestsAndMissingSubtitle(t *testing.T) {
	c := NewClient(nil)
	t.Cleanup(c.Close)
	masterURL, subtitleURL, err := c.publish([]byte("master"), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		method string
		url    string
		status int
	}{
		{http.MethodGet, strings.Replace(masterURL, "/master.m3u8", "/subtitle.vtt", 1), http.StatusNotFound},
		{http.MethodGet, c.assets.baseURL + "/not-a-token/master.m3u8", http.StatusNotFound},
		{http.MethodPost, masterURL, http.StatusMethodNotAllowed},
		{http.MethodGet, masterURL + "/extra", http.StatusNotFound},
	} {
		request, _ := http.NewRequest(test.method, test.url, nil)
		response, requestErr := http.DefaultClient.Do(request)
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		response.Body.Close()
		if response.StatusCode != test.status {
			t.Errorf("%s %s: status=%d, want %d", test.method, test.url, response.StatusCode, test.status)
		}
	}
	if subtitleURL != "" {
		t.Fatal("subtitle URL published for empty subtitle")
	}
	c.Close()
}

func TestAssetsConcurrentUseAndBound(t *testing.T) {
	c := NewClient(nil)
	t.Cleanup(c.Close)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 8; j++ {
				masterURL, _, err := c.publish([]byte("master"), []byte("sub"))
				if err != nil {
					t.Errorf("publish: %v", err)
					return
				}
				response, err := http.Get(masterURL)
				if err != nil {
					t.Errorf("GET: %v", err)
					return
				}
				response.Body.Close()
			}
		}()
	}
	wg.Wait()
	c.assets.mu.RLock()
	count := len(c.assets.entries)
	c.assets.mu.RUnlock()
	if count > maxAssetEntries {
		t.Fatalf("asset count = %d, want <= %d", count, maxAssetEntries)
	}
	firstURL, _, err := c.publish([]byte("first"), nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxAssetEntries; i++ {
		if _, _, err := c.publish([]byte("next"), nil); err != nil {
			t.Fatal(err)
		}
	}
	response, err := http.Get(firstURL)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("evicted asset status = %d, want %d", response.StatusCode, http.StatusNotFound)
	}
	c.Close()
}

func TestCloseIsIdempotentAndPreventsReopen(t *testing.T) {
	c := NewClient(nil)
	t.Cleanup(c.Close)
	if _, _, err := c.publish([]byte("master"), nil); err != nil {
		t.Fatal(err)
	}
	c.Close()
	c.Close()
	if _, _, err := c.publish([]byte("master"), nil); err == nil {
		t.Fatal("publish after Close succeeded")
	}
}
