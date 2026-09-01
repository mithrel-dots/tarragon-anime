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
}

type Client interface {
	Match(context.Context, int, []string, string) (Anime, error)
	Episodes(context.Context, Anime, string) ([]Episode, error)
	Streams(context.Context, Episode, string, string) ([]Stream, error)
}
