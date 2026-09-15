package anineko

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

var pngIENDMarker = []byte{0x49, 0x45, 0x4e, 0x44, 0xae, 0x42, 0x60, 0x82}

type vibeSegment struct {
	duration float64
	url      string
}

type vibeSession struct {
	masterURL string
	referer   string
	variants  map[string]string
	segments  map[string][]vibeSegment
}

type vibeProxy struct {
	client   *http.Client
	baseURL  string
	server   *http.Server
	listener net.Listener

	mu       sync.Mutex
	sessions map[string]*vibeSession
}

func (c *Client) vibeProxy() (*vibeProxy, error) {
	c.proxyMu.Lock()
	defer c.proxyMu.Unlock()
	if c.proxy != nil {
		return c.proxy, nil
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("start AniNeko stream proxy: %w", err)
	}
	proxy := &vibeProxy{
		client:   c.http,
		baseURL:  "http://" + listener.Addr().String(),
		listener: listener,
		sessions: map[string]*vibeSession{},
	}
	proxy.server = &http.Server{Handler: http.HandlerFunc(proxy.handle)}
	go func() { _ = proxy.server.Serve(listener) }()
	c.proxy = proxy
	return proxy, nil
}

func (p *vibeProxy) close() {
	if p.server != nil {
		_ = p.server.Close()
	}
	if p.listener != nil {
		_ = p.listener.Close()
	}
}

func (p *vibeProxy) register(masterURL, referer string) (string, error) {
	var rawID [8]byte
	if _, err := rand.Read(rawID[:]); err != nil {
		return "", err
	}
	id := hex.EncodeToString(rawID[:])
	p.mu.Lock()
	p.sessions[id] = &vibeSession{
		masterURL: masterURL,
		referer:   referer,
		variants:  map[string]string{},
		segments:  map[string][]vibeSegment{},
	}
	p.mu.Unlock()
	return p.baseURL + "/stream/" + id + "/master.m3u8", nil
}

func (p *vibeProxy) handle(w http.ResponseWriter, request *http.Request) {
	parts := strings.Split(strings.Trim(strings.TrimPrefix(request.URL.Path, "/stream/"), "/"), "/")
	if len(parts) < 2 {
		http.NotFound(w, request)
		return
	}
	p.mu.Lock()
	session := p.sessions[parts[0]]
	p.mu.Unlock()
	if session == nil {
		http.NotFound(w, request)
		return
	}
	switch {
	case len(parts) == 2 && parts[1] == "master.m3u8":
		p.serveMaster(w, request.Context(), parts[0], session)
	case len(parts) == 2 && strings.HasSuffix(parts[1], ".m3u8"):
		p.serveVariant(w, request.Context(), parts[0], parts[1], session)
	case len(parts) == 4 && parts[1] == "seg":
		p.serveSegment(w, request.Context(), parts[2], parts[3], session)
	default:
		http.NotFound(w, request)
	}
}

func (p *vibeProxy) serveMaster(w http.ResponseWriter, ctx context.Context, sessionID string, session *vibeSession) {
	body, err := p.fetch(ctx, session.masterURL, session.referer)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	variantIndex := 0
	rewritten := rewritePlaylistLines(string(body), func(line string) string {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			return line
		}
		variantName := fmt.Sprintf("variant-%d.m3u8", variantIndex)
		variantIndex++
		p.mu.Lock()
		session.variants[variantName] = resolvePlaylistURL(session.masterURL, line)
		p.mu.Unlock()
		return p.baseURL + "/stream/" + sessionID + "/" + variantName
	})
	writePlaylist(w, rewritten)
}

func (p *vibeProxy) serveVariant(w http.ResponseWriter, ctx context.Context, sessionID, variantName string, session *vibeSession) {
	p.mu.Lock()
	variantURL := session.variants[variantName]
	p.mu.Unlock()
	if variantURL == "" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	body, err := p.fetch(ctx, variantURL, session.referer)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	segments := make([]vibeSegment, 0)
	var duration float64
	segmentIndex := 0
	rewritten := rewritePlaylistLines(string(body), func(line string) string {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#EXTINF:") {
			duration = parseExtInfDuration(line)
			return line
		}
		if line == "" || strings.HasPrefix(line, "#") {
			return line
		}
		segments = append(segments, vibeSegment{
			duration: duration,
			url:      resolvePlaylistURL(variantURL, line),
		})
		current := segmentIndex
		segmentIndex++
		return fmt.Sprintf("%s/stream/%s/seg/%s/%d", p.baseURL, sessionID, variantName, current)
	})

	p.mu.Lock()
	session.segments[variantName] = segments
	p.mu.Unlock()
	writePlaylist(w, rewritten)
}

func (p *vibeProxy) serveSegment(w http.ResponseWriter, ctx context.Context, variantName, indexText string, session *vibeSession) {
	index, err := strconv.Atoi(indexText)
	if err != nil || index < 0 {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	p.mu.Lock()
	segments := session.segments[variantName]
	p.mu.Unlock()
	if index >= len(segments) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	body, err := p.fetch(ctx, segments[index].url, session.referer)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	body = stripPNGWrapper(body)
	w.Header().Set("Content-Type", "video/mp2t")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	_, _ = w.Write(body)
}

func (p *vibeProxy) fetch(ctx context.Context, endpoint, referer string) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	setHeaders(request, referer)
	response, err := p.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 32<<10))
		return nil, fmt.Errorf("HTTP status %s: %s", response.Status, compactBody(body))
	}
	return io.ReadAll(io.LimitReader(response.Body, maxBody))
}

func rewritePlaylistLines(playlist string, mapLine func(string) string) string {
	lines := strings.Split(strings.ReplaceAll(playlist, "\r\n", "\n"), "\n")
	for index, line := range lines {
		lines[index] = mapLine(line)
	}
	return strings.Join(lines, "\n")
}

func parseExtInfDuration(line string) float64 {
	line = strings.TrimPrefix(strings.TrimSpace(line), "#EXTINF:")
	if comma := strings.Index(line, ","); comma >= 0 {
		line = line[:comma]
	}
	value, err := strconv.ParseFloat(strings.TrimSpace(line), 64)
	if err != nil {
		return 0
	}
	return value
}

func stripPNGWrapper(body []byte) []byte {
	index := bytes.Index(body, pngIENDMarker)
	if index < 0 {
		return body
	}
	return body[index+len(pngIENDMarker):]
}

func writePlaylist(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	_, _ = io.WriteString(w, body)
}
