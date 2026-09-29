package anipub

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const maxProxyEntries = 32

type proxyEntry struct {
	playlist  string
	resources map[string]string
	headers   map[string]string
}

type mediaProxy struct {
	client  *http.Client
	server  *http.Server
	baseURL string
	mu      sync.RWMutex
	entries map[string]proxyEntry
	order   []string
}

// Publication belongs to the client, not to the lookup context. Cancelling
// losing provider lookups must not stop the selected stream's local server.
func (c *Client) publish(ctx context.Context, playlist string, resources, headers map[string]string) (string, error) {
	c.proxyMu.Lock()
	defer c.proxyMu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if c.closed {
		return "", errors.New("AniPub client is closed")
	}
	if c.proxy == nil {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return "", err
		}
		client := *c.http
		client.Jar = nil // Playback never needs metadata or player cookies.
		p := &mediaProxy{client: &client, baseURL: "http://" + listener.Addr().String(), entries: make(map[string]proxyEntry)}
		p.server = &http.Server{Handler: http.HandlerFunc(p.handle), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, WriteTimeout: 45 * time.Second}
		c.proxy = p
		go func() { _ = p.server.Serve(listener) }()
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	token := hex.EncodeToString(random[:])
	p := c.proxy
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	p.entries[token] = proxyEntry{playlist, maps.Clone(resources), maps.Clone(headers)}
	p.order = append(p.order, token)
	for len(p.order) > maxProxyEntries {
		delete(p.entries, p.order[0])
		p.order = p.order[1:]
	}
	return p.baseURL + "/" + token + "/stream.m3u8", nil
}

func (c *Client) Close() {
	c.proxyMu.Lock()
	c.closed = true
	p := c.proxy
	c.proxy = nil
	c.proxyMu.Unlock()
	if p != nil {
		_ = p.server.Close()
	}
}

func (p *mediaProxy) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	p.mu.RLock()
	entry, ok := p.entries[parts[0]]
	p.mu.RUnlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	if parts[1] == "stream.m3u8" {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Header().Set("Cache-Control", "no-store")
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, entry.playlist)
		}
		return
	}
	endpoint := entry.resources[parts[1]]
	if endpoint == "" {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, r.Method, endpoint, nil)
	if err != nil {
		http.Error(w, "invalid media request", http.StatusBadGateway)
		return
	}
	for _, key := range []string{"Referer", "Origin", "User-Agent"} {
		if value := entry.headers[key]; value != "" {
			req.Header.Set(key, value)
		}
	}
	if value := r.Header.Get("Range"); value != "" {
		req.Header.Set("Range", value)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		http.Error(w, "media upstream unavailable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		http.Error(w, fmt.Sprintf("media upstream status %d", resp.StatusCode), http.StatusBadGateway)
		return
	}
	for _, key := range []string{"Content-Length", "Content-Range", "Accept-Ranges"} {
		if value := resp.Header.Get(key); value != "" {
			w.Header().Set(key, value)
		}
	}
	contentType := "video/mp2t"
	if strings.HasSuffix(parts[1], ".key") {
		contentType = "application/octet-stream"
	} else if strings.HasSuffix(parts[1], ".mp4") || strings.HasSuffix(parts[1], ".m4s") {
		contentType = "video/mp4"
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(resp.StatusCode)
	if r.Method == http.MethodGet {
		_, _ = io.Copy(w, resp.Body)
	}
}
