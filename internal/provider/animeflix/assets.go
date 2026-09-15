package animeflix

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const maxAssetEntries = 128

type assetEntry struct {
	master   []byte
	subtitle []byte
}

type assetServer struct {
	listener net.Listener
	server   *http.Server
	baseURL  string

	mu      sync.RWMutex
	entries map[string]assetEntry
	order   []string
}

// publish makes the already-normalized playlists available locally. It never
// fetches media: URLs in master are intentionally left as absolute origins.
func (c *Client) publish(master, subtitle []byte) (masterURL string, subtitleURL string, err error) {
	c.assetsMu.Lock()
	defer c.assetsMu.Unlock()
	if c.closed {
		return "", "", errors.New("AnimeFlix client is closed")
	}

	if c.assets == nil {
		listener, listenErr := net.Listen("tcp", "127.0.0.1:0")
		if listenErr != nil {
			return "", "", fmt.Errorf("start AnimeFlix asset server: %w", listenErr)
		}
		assets := &assetServer{
			listener: listener,
			baseURL:  "http://" + listener.Addr().String(),
			entries:  make(map[string]assetEntry),
		}
		assets.server = &http.Server{
			Handler:           http.HandlerFunc(assets.handle),
			ReadHeaderTimeout: 5 * time.Second,
			IdleTimeout:       30 * time.Second,
			WriteTimeout:      10 * time.Second,
		}
		c.assets = assets
		go func() { _ = assets.server.Serve(listener) }()
	}

	token, tokenErr := assetToken()
	if tokenErr != nil {
		return "", "", tokenErr
	}
	c.assets.mu.Lock()
	c.assets.entries[token] = assetEntry{master: append([]byte(nil), master...), subtitle: append([]byte(nil), subtitle...)}
	c.assets.order = append(c.assets.order, token)
	for len(c.assets.order) > maxAssetEntries {
		oldest := c.assets.order[0]
		c.assets.order = c.assets.order[1:]
		delete(c.assets.entries, oldest)
	}
	c.assets.mu.Unlock()

	masterURL = c.assets.baseURL + "/" + token + "/master.m3u8"
	if len(subtitle) != 0 {
		subtitleURL = c.assets.baseURL + "/" + token + "/subtitle.vtt"
	}
	return masterURL, subtitleURL, nil
}

// Close stops the lazy local server and permanently closes the client to asset
// publication. Existing requests may finish as part of http.Server.Close.
func (c *Client) Close() {
	c.assetsMu.Lock()
	if c.closed {
		c.assetsMu.Unlock()
		return
	}
	c.closed = true
	assets := c.assets
	c.assets = nil
	c.assetsMu.Unlock()
	if assets != nil {
		_ = assets.server.Close()
		_ = assets.listener.Close()
	}
}

func assetToken() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate AnimeFlix asset token: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

func (a *assetServer) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if len(parts) != 2 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}

	a.mu.RLock()
	entry, ok := a.entries[parts[0]]
	a.mu.RUnlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	var body []byte
	var contentType string
	switch parts[1] {
	case "master.m3u8":
		body, contentType = entry.master, "application/vnd.apple.mpegurl"
	case "subtitle.vtt":
		if len(entry.subtitle) == 0 {
			http.NotFound(w, r)
			return
		}
		body, contentType = entry.subtitle, "text/vtt"
	default:
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write(body)
	}
}
