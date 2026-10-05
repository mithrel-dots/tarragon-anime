// Package provider contains the provider-neutral contract used by the anime
// service. Provider implementations may keep their own internal models, but
// expose these values at the service boundary.
package provider

import "context"

type Anime struct {
	ID        string
	Name      string
	English   string
	AniListID int
}

type Episode struct {
	ShowID string
	Number int
	Value  string
}

type Stream struct {
	URL      string
	Headers  map[string]string
	Subtitle string
	Source   string // Non-secret source label or ordinal for resolution logs.
}

type Client interface {
	Match(context.Context, int, []string, string) (Anime, error)
	Episodes(context.Context, Anime, string) ([]Episode, error)
	Streams(context.Context, Episode, string, string) ([]Stream, error)
}

// EpisodeLookup is an optional capability for providers with explicit episode
// routes that can outlive a stale catalogue. LookupEpisode must preserve the
// matched show's identity and requested number; Streams still validates that
// the addressed episode has playable media. Opaque IDs must not be guessed.
type EpisodeLookup interface {
	LookupEpisode(context.Context, Anime, int, string) (Episode, error)
}

// MALEpisodeLookup is an optional fallback when a title is absent from a
// provider's catalogue but its player accepts MyAnimeList IDs. The ID must come
// from AniList metadata, and the provider must confirm that the player route
// retains that ID, episode and translation. Streams still validates the media.
type MALEpisodeLookup interface {
	LookupMALEpisode(context.Context, int, int, string) (Episode, error)
}
