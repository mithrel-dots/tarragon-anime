package anipub

import (
	"context"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"time"
)

// streamTTL bounds reuse of native and browser resolutions. Upstream URLs may
// expire; the short cache also keeps repeated resolution off the playback path.
const streamTTL = 5 * time.Minute

type streamEntry struct {
	streams []Stream
	expires time.Time
}

func streamKey(episode Episode, translation, quality string) string {
	if episode.Value == "" {
		episode.Value = strconv.Itoa(episode.Number)
	}
	translation = strings.ToLower(strings.TrimSpace(translation))
	if translation == "" {
		translation = "sub"
	}
	return fmt.Sprintf("%s|%s|%s|%s", episode.ShowID, episode.Value, translation, quality)
}

func (c *Client) cachedStreams(episode Episode, translation, quality string) ([]Stream, bool) {
	key := streamKey(episode, translation, quality)
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()
	entry, ok := c.streamCache[key]
	if !ok || time.Now().After(entry.expires) {
		delete(c.streamCache, key)
		return nil, false
	}
	return cloneStreams(entry.streams), true
}

func (c *Client) cacheStreams(ctx context.Context, episode Episode, translation, quality string, streams []Stream) error {
	if len(streams) == 0 {
		return ctx.Err()
	}
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()
	entry := streamEntry{
		streams: cloneStreams(streams),
		expires: time.Now().Add(streamTTL),
	}
	// A cancelled loser must neither replace nor invalidate a winner's entry.
	if err := ctx.Err(); err != nil {
		return err
	}
	c.streamCache[streamKey(episode, translation, quality)] = entry
	return nil
}

func cloneStreams(streams []Stream) []Stream {
	out := append([]Stream(nil), streams...)
	for i := range out {
		out[i].Headers = maps.Clone(out[i].Headers)
	}
	return out
}

// InvalidateStreams drops a cached capture. The service calls it when playback
// fails, so an expired signed URL is re-resolved instead of being handed to the
// player again.
func (c *Client) InvalidateStreams(episode Episode, translation, quality string) {
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()
	delete(c.streamCache, streamKey(episode, translation, quality))
}
