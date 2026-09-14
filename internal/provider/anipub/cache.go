package anipub

import (
	"fmt"
	"time"
)

// streamTTL is how long a capture is reused. The captured URLs are signed and
// expire, and a capture costs a full page load, so the window is short enough
// to stay valid and long enough to keep next-episode prefetch off the critical
// path.
const streamTTL = 5 * time.Minute

type streamEntry struct {
	streams []Stream
	expires time.Time
}

func streamKey(episode Episode, translation, quality string) string {
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
	return append([]Stream(nil), entry.streams...), true
}

func (c *Client) cacheStreams(episode Episode, translation, quality string, streams []Stream) {
	if len(streams) == 0 {
		return
	}
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()
	c.streamCache[streamKey(episode, translation, quality)] = streamEntry{
		streams: append([]Stream(nil), streams...),
		expires: time.Now().Add(streamTTL),
	}
}

// InvalidateStreams drops a cached capture. The service calls it when playback
// fails, so an expired signed URL is re-resolved instead of being handed to the
// player again.
func (c *Client) InvalidateStreams(episode Episode, translation, quality string) {
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()
	delete(c.streamCache, streamKey(episode, translation, quality))
}
