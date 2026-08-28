package anime

import (
	"context"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"

	"tarragon-anime/internal/anilist"
	"tarragon-anime/internal/mpv"
	"tarragon-anime/internal/provider/allanime"
)

type aniListClient interface {
	Search(context.Context, string) ([]anilist.Media, error)
	Get(context.Context, int) (anilist.Media, error)
}

type providerClient interface {
	Match(context.Context, int, []string, string) (allanime.Anime, error)
	Episodes(context.Context, allanime.Anime, string) ([]allanime.Episode, error)
	Streams(context.Context, allanime.Episode, string, string) ([]allanime.Stream, error)
}

type player interface {
	Play(context.Context, mpv.Stream, string) error
}

type Service struct {
	anilist  aniListClient
	provider providerClient
	player   player
	config   Config
	logger   *log.Logger

	mediaMu sync.RWMutex
	media   map[int]anilist.Media
}

func NewService(anilistClient aniListClient, provider providerClient, player player, config Config, logger *log.Logger) *Service {
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	return &Service{
		anilist: anilistClient, provider: provider, player: player, config: config,
		logger: logger, media: make(map[int]anilist.Media),
	}
}

func (s *Service) Search(ctx context.Context, query string) ([]Media, error) {
	if strings.TrimSpace(query) == "" {
		return nil, nil
	}
	items, err := s.anilist.Search(ctx, query)
	if err != nil {
		return nil, err
	}
	result := make([]Media, 0, len(items))
	s.mediaMu.Lock()
	for _, item := range items {
		s.media[item.ID] = item
		result = append(result, mediaFromAniList(item))
	}
	s.mediaMu.Unlock()
	return result, nil
}

func (s *Service) Episodes(ctx context.Context, mediaID int) ([]Episode, error) {
	media, providerAnime, err := s.resolveProvider(ctx, mediaID)
	if err != nil {
		return nil, err
	}
	items, err := s.provider.Episodes(ctx, providerAnime, s.config.Translation)
	if err != nil {
		return nil, fmt.Errorf("resolve AllAnime episodes for AniList %d: %w", mediaID, err)
	}
	s.logger.Printf("provider episodes provider=allanime media_id=%d provider_id=%s count=%d", mediaID, providerAnime.ID, len(items))
	result := make([]Episode, 0, len(items))
	for _, item := range items {
		result = append(result, Episode{
			MediaID: mediaID, Number: item.Number,
			Title:    fmt.Sprintf("%s - Episode %d", media.Title, item.Number),
			Provider: "allanime", ProviderID: providerAnime.ID,
		})
	}
	return result, nil
}

func (s *Service) Play(ctx context.Context, mediaID, episodeNumber int) error {
	episodes, err := s.Episodes(ctx, mediaID)
	if err != nil {
		return err
	}
	for _, episode := range episodes {
		if episode.Number == episodeNumber {
			return s.PlayEpisode(ctx, episode)
		}
	}
	return fmt.Errorf("episode %d is not available for AniList %d", episodeNumber, mediaID)
}

func (s *Service) PlayEpisode(ctx context.Context, episode Episode) error {
	if episode.Provider != "allanime" || episode.ProviderID == "" {
		return fmt.Errorf("invalid provider context for %s", episode.ResultID())
	}
	streams, err := s.provider.Streams(ctx, allanime.Episode{
		ShowID: episode.ProviderID, Number: episode.Number, Value: fmt.Sprint(episode.Number),
	}, s.config.Translation, s.config.PreferredQuality)
	if err != nil {
		return fmt.Errorf("resolve AllAnime episode %d: %w", episode.Number, err)
	}
	if len(streams) == 0 {
		return fmt.Errorf("resolve AllAnime episode %d: no playable streams", episode.Number)
	}
	stream := streams[0]
	s.logger.Printf("stream resolved provider=allanime media_id=%d episode=%d", episode.MediaID, episode.Number)
	if err := s.player.Play(ctx, mpv.Stream{URL: stream.URL, Headers: stream.Headers, Subtitle: stream.Subtitle}, episode.Title); err != nil {
		return fmt.Errorf("launch mpv: %w", err)
	}
	return nil
}

func (s *Service) resolveProvider(ctx context.Context, mediaID int) (anilist.Media, allanime.Anime, error) {
	s.mediaMu.RLock()
	media, ok := s.media[mediaID]
	s.mediaMu.RUnlock()
	if !ok {
		var err error
		media, err = s.anilist.Get(ctx, mediaID)
		if err != nil {
			return anilist.Media{}, allanime.Anime{}, err
		}
		s.mediaMu.Lock()
		s.media[mediaID] = media
		s.mediaMu.Unlock()
	}
	aliases := []string{media.English, media.Romaji, media.Native, media.Title}
	aliases = append(aliases, media.Synonyms...)
	match, err := s.provider.Match(ctx, mediaID, aliases, s.config.Translation)
	if err != nil {
		return anilist.Media{}, allanime.Anime{}, fmt.Errorf("match provider title for AniList %d: %w", mediaID, err)
	}
	s.logger.Printf("provider match provider=allanime media_id=%d provider_id=%s", mediaID, match.ID)
	return media, match, nil
}

func mediaFromAniList(item anilist.Media) Media {
	aliases := []string{item.English, item.Romaji, item.Native}
	aliases = append(aliases, item.Synonyms...)
	return Media{
		ID: item.ID, Title: item.Title, Format: item.Format, Episodes: item.Episodes,
		CoverURL: item.CoverURL, Aliases: aliases,
	}
}
