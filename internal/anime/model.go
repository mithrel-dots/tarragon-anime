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

type Stream struct {
	URL      string
	Headers  map[string]string
	Subtitle string
}
