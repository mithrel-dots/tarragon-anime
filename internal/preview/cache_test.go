package preview

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestCacheDownloadsAndReusesCover(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("Accept") == "" {
			t.Error("missing image Accept header")
		}
		_, _ = w.Write([]byte("fake-image"))
	}))
	defer server.Close()

	dir := filepath.Join(t.TempDir(), "covers")
	cache := NewCache(server.Client(), dir)
	first, err := cache.Get(context.Background(), 154587, server.URL+"/cover.jpg")
	if err != nil {
		t.Fatal(err)
	}
	second, err := cache.Get(context.Background(), 154587, server.URL+"/cover.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if first != second || requests != 1 {
		t.Fatalf("paths = %q, %q; requests = %d", first, second, requests)
	}
	data, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "fake-image" {
		t.Fatalf("cached data = %q", data)
	}
}

func TestCacheRejectsOversizedCover(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(make([]byte, maxImageSize+1))
	}))
	defer server.Close()
	_, err := NewCache(server.Client(), t.TempDir()).Get(context.Background(), 1, server.URL)
	if err == nil {
		t.Fatal("Get() error = nil")
	}
}
