package tarragon

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"

	"tarragon-anime/internal/anime"
)

type animeService interface {
	Search(context.Context, string) ([]anime.Media, error)
	Episodes(context.Context, int) ([]anime.Episode, error)
	Play(context.Context, int, int) error
	PlayEpisode(context.Context, anime.Episode) error
}

type Plugin struct {
	service     animeService
	logger      *log.Logger
	pluginID    string
	queryPrefix string

	mu         sync.Mutex
	selections map[string]map[string]anime.Episode
}

func NewPlugin(service animeService, pluginID, prefix, prefixSymbol string, logger *log.Logger) *Plugin {
	if pluginID == "" {
		pluginID = "anime"
	}
	if prefix == "" {
		prefix = pluginID
	}
	if prefixSymbol == "" {
		prefixSymbol = "@"
	}
	if !strings.HasPrefix(prefix, prefixSymbol) {
		prefix = prefixSymbol + prefix
	}
	return &Plugin{
		service: service, logger: logger, pluginID: pluginID, queryPrefix: prefix,
		selections: make(map[string]map[string]anime.Episode),
	}
}

func (p *Plugin) Request(ctx context.Context, queryID, text string) Payload {
	query, err := anime.ParseQuery(text)
	if err != nil {
		return errorPayload(err)
	}
	switch query.Command {
	case anime.CommandSearch:
		media, err := p.service.Search(ctx, query.Text)
		if err != nil {
			return errorPayload(err)
		}
		results := make([]Result, 0, len(media))
		for index, item := range media {
			description := item.Format
			if item.Episodes > 0 {
				description = fmt.Sprintf("%s | %d episodes", item.Format, item.Episodes)
			}
			results = append(results, Result{
				ID: fmt.Sprintf("media:%d", item.ID), Label: item.Title,
				Description: description, Score: resultScore(index), Icon: "video-x-generic",
				Category: "anime", PreviewPath: item.CoverURL,
				Actions: []Action{{Name: "Episodes", Type: "query_replace", Query: fmt.Sprintf("%s episodes %d", p.queryPrefix, item.ID)}},
			})
		}
		return Payload{Results: results}
	case anime.CommandEpisodes:
		episodes, err := p.service.Episodes(ctx, query.MediaID)
		if err != nil {
			return errorPayload(err)
		}
		selection := make(map[string]anime.Episode, len(episodes))
		results := make([]Result, 0, len(episodes))
		for index, episode := range episodes {
			selection[episode.ResultID()] = episode
			results = append(results, Result{
				ID: episode.ResultID(), Label: fmt.Sprintf("Episode %d", episode.Number),
				Description: episode.Title, Score: resultScore(index), Icon: "media-playback-start",
				Category: "anime", Actions: []Action{{Name: "play"}},
			})
		}
		p.storeSelection(queryID, selection)
		return Payload{Results: results}
	case anime.CommandPlay:
		if err := p.service.Play(ctx, query.MediaID, query.Episode); err != nil {
			return errorPayload(err)
		}
		return Payload{Results: []Result{{ID: "playback:started", Label: "Started playback", Category: "anime"}}}
	default:
		return errorPayload(fmt.Errorf("unsupported query"))
	}
}

func (p *Plugin) Select(ctx context.Context, message Message) (bool, string) {
	if message.Plugin != "" && message.Plugin != p.pluginID {
		return false, "Selection was routed to the wrong plugin"
	}
	if message.Action != "play" {
		return false, fmt.Sprintf("Unsupported action %q", message.Action)
	}
	p.mu.Lock()
	results := p.selections[message.QueryID]
	episode, ok := results[message.ResultID]
	p.mu.Unlock()
	if !ok {
		return false, "Episode selection is stale; open Episodes again"
	}
	if err := p.service.PlayEpisode(ctx, episode); err != nil {
		p.logger.Printf("playback failed result_id=%s error=%v", message.ResultID, err)
		return false, err.Error()
	}
	return true, "Started playback"
}

func (p *Plugin) storeSelection(queryID string, selection map[string]anime.Episode) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.selections) >= 128 {
		for key := range p.selections {
			delete(p.selections, key)
			break
		}
	}
	p.selections[queryID] = selection
}

func resultScore(index int) float64 {
	score := 1 - float64(index)*0.01
	if score < 0.01 {
		return 0.01
	}
	return score
}

func errorPayload(err error) Payload {
	return Payload{Results: []Result{{
		ID: "error", Label: "Anime plugin error", Description: err.Error(),
		Score: 1, Icon: "dialog-error", Category: "anime",
	}}}
}
