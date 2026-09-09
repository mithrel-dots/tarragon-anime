package allanime

import (
	"strings"
	"time"
)

// streamTTL is deliberately short. Upstream playback URLs carry signed,
// expiring tokens whose lifetime is not advertised in a form worth parsing, so
// a cached answer is only reused for as long as a viewer is plausibly still
// working through the same session.
const streamTTL = 10 * time.Minute

type streamEntry struct {
	streams []Stream
	expires time.Time
}

// streamKey identifies a cached resolution. Quality is part of the key because
// a "worst" request and a "best" request select different variants from the
// same capture.
func streamKey(episode Episode, translation, quality string) string {
	return strings.Join([]string{
		episode.ShowID,
		episode.Value,
		strings.ToLower(translation),
		strings.ToLower(quality),
	}, "\x00")
}

func (c *Client) cachedStreams(episode Episode, translation, quality string) ([]Stream, bool) {
	key := streamKey(episode, translation, quality)
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()
	entry, ok := c.streamCache[key]
	if !ok {
		return nil, false
	}
	if !c.now().Before(entry.expires) {
		delete(c.streamCache, key)
		return nil, false
	}
	return cloneStreams(entry.streams), true
}

func (c *Client) cacheStreams(episode Episode, translation, quality string, streams []Stream) {
	if len(streams) == 0 {
		return
	}
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()
	if c.streamCache == nil {
		c.streamCache = make(map[string]streamEntry)
	}
	c.streamCache[streamKey(episode, translation, quality)] = streamEntry{
		streams: cloneStreams(streams),
		expires: c.now().Add(streamTTL),
	}
}

// InvalidateStreams drops a cached resolution. Callers use it when a stream
// that resolved successfully turned out to be unplayable, so the next attempt
// goes back to the provider instead of replaying a dead URL.
func (c *Client) InvalidateStreams(episode Episode, translation, quality string) {
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()
	delete(c.streamCache, streamKey(episode, translation, quality))
}

func cloneStreams(streams []Stream) []Stream {
	out := make([]Stream, 0, len(streams))
	for _, stream := range streams {
		copied := stream
		if stream.Headers != nil {
			copied.Headers = make(map[string]string, len(stream.Headers))
			for key, value := range stream.Headers {
				copied.Headers[key] = value
			}
		}
		out = append(out, copied)
	}
	return out
}
