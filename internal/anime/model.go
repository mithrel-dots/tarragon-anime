package anime

import "fmt"

type Media struct {
	ID       int
	Title    string
	Format   string
	Episodes int
	CoverURL string
	Aliases  []string
}

type ListItem struct {
	Media    Media
	Status   string
	Progress int
}

type Episode struct {
	MediaID    int
	Number     int
	Title      string
	Provider   string
	ProviderID string
}

func (e Episode) ResultID() string {
	return fmt.Sprintf("episode:%d:%d", e.MediaID, e.Number)
}

// Resume describes the next episode to play for a previously watched anime.
type Resume struct {
	MediaID       int
	Title         string
	PreviewPath   string
	Episode       int
	Position      float64
	TotalEpisodes int
	Continues     bool
}

type Stream struct {
	URL      string
	Headers  map[string]string
	Subtitle string
}
