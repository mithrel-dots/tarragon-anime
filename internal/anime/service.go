package anime

import (
	"context"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"time"

	"tarragon-anime/internal/anilist"
	"tarragon-anime/internal/mpv"
	"tarragon-anime/internal/provider/allanime"
	"tarragon-anime/internal/store"
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
	Play(context.Context, mpv.Stream, string, float64) (mpv.SessionController, error)
}

type stateStore interface {
	ProviderMapping(context.Context, int, string) (string, bool, error)
	SaveProviderMapping(context.Context, int, string, string) error
	Progress(context.Context, int, int) (store.Progress, bool, error)
	SaveProgress(context.Context, store.Progress) error
}

type Service struct {
	anilist  aniListClient
	provider providerClient
	player   player
	state    stateStore
	config   Config
	logger   *log.Logger

	cacheMu  sync.RWMutex
	media    map[int]anilist.Media
	episodes map[int][]Episode

	playbackMu sync.Mutex
	active     *activePlayback
}

type activePlayback struct {
	ctx      context.Context
	cancel   context.CancelFunc
	session  mpv.SessionController
	episodes []Episode
	index    int
	stream   mpv.Stream

	position        float64
	duration        float64
	lastSaved       time.Time
	subtitlePending bool
	complete        bool
	closeOnce       sync.Once
}

func NewService(anilistClient aniListClient, provider providerClient, player player, state stateStore, config Config, logger *log.Logger) *Service {
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	return &Service{
		anilist: anilistClient, provider: provider, player: player, state: state,
		config: config, logger: logger, media: make(map[int]anilist.Media),
		episodes: make(map[int][]Episode),
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
	s.cacheMu.Lock()
	for _, item := range items {
		s.media[item.ID] = item
		result = append(result, mediaFromAniList(item))
	}
	s.cacheMu.Unlock()
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
	s.cacheMu.Lock()
	s.episodes[mediaID] = append([]Episode(nil), result...)
	s.cacheMu.Unlock()
	return result, nil
}

func (s *Service) Play(ctx context.Context, mediaID, episodeNumber int) error {
	episodes, err := s.availableEpisodes(ctx, mediaID)
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
	episodes, err := s.availableEpisodes(ctx, episode.MediaID)
	if err != nil {
		return err
	}
	index := episodeIndex(episodes, episode.Number)
	if index < 0 {
		return fmt.Errorf("episode %d is not available for AniList %d", episode.Number, episode.MediaID)
	}
	stream, err := s.resolveStream(ctx, episode)
	if err != nil {
		return err
	}
	start := s.resumePosition(ctx, episode)
	session, err := s.player.Play(ctx, stream, episode.Title, start)
	if err != nil {
		return fmt.Errorf("launch mpv: %w", err)
	}
	playbackCtx, cancel := context.WithCancel(ctx)
	active := &activePlayback{
		ctx: playbackCtx, cancel: cancel, session: session,
		episodes: episodes, index: index, stream: stream,
		position: start, lastSaved: time.Now(),
	}

	s.playbackMu.Lock()
	previous := s.active
	s.active = active
	s.playbackMu.Unlock()
	if previous != nil {
		previous.stop()
	}
	go s.controlPlayback(active)
	return nil
}

func (s *Service) Close() {
	s.playbackMu.Lock()
	active := s.active
	s.active = nil
	s.playbackMu.Unlock()
	if active != nil {
		active.stop()
	}
}

func (s *Service) controlPlayback(active *activePlayback) {
	defer func() {
		s.saveProgress(active, active.complete)
		s.playbackMu.Lock()
		if s.active == active {
			s.active = nil
		}
		s.playbackMu.Unlock()
	}()
	for {
		select {
		case <-active.ctx.Done():
			active.stop()
			return
		case event, ok := <-active.session.Events():
			if !ok {
				return
			}
			s.handlePlaybackEvent(active, event)
		}
	}
}

func (s *Service) handlePlaybackEvent(active *activePlayback, event mpv.Event) {
	switch event.Type {
	case mpv.EventPosition:
		active.position = event.Value
		if !active.complete && time.Since(active.lastSaved) >= 10*time.Second {
			s.saveProgress(active, false)
		}
	case mpv.EventDuration:
		active.duration = event.Value
	case mpv.EventFileLoaded:
		if active.subtitlePending && active.stream.Subtitle != "" {
			if err := active.session.AddSubtitle(active.ctx, active.stream.Subtitle); err != nil {
				s.logger.Printf("add subtitle: %v", err)
			}
		}
		active.subtitlePending = false
	case mpv.EventEndFile:
		complete := event.Reason == "eof"
		s.saveProgress(active, complete)
		if complete {
			if s.config.AutoNext && active.index+1 < len(active.episodes) {
				s.navigate(active, 1, false)
			} else {
				active.stop()
			}
		}
	case mpv.EventNext:
		s.navigate(active, 1, true)
	case mpv.EventPrevious:
		s.navigate(active, -1, true)
	case mpv.EventShutdown:
		active.cancel()
	}
}

func (p *activePlayback) stop() {
	p.closeOnce.Do(func() {
		p.cancel()
		p.session.Close()
	})
}

func (s *Service) navigate(active *activePlayback, delta int, saveCurrent bool) {
	target := active.index + delta
	if target < 0 || target >= len(active.episodes) {
		_ = active.session.ShowText(active.ctx, "No adjacent episode available")
		return
	}
	if saveCurrent && !active.complete {
		s.saveProgress(active, false)
	}
	episode := active.episodes[target]
	stream, err := s.resolveStream(active.ctx, episode)
	if err != nil {
		s.logger.Printf("navigate to episode %d: %v", episode.Number, err)
		_ = active.session.ShowText(active.ctx, "Could not resolve episode "+fmt.Sprint(episode.Number))
		return
	}
	start := s.resumePosition(active.ctx, episode)
	if err := active.session.Load(active.ctx, stream, episode.Title, start); err != nil {
		s.logger.Printf("load episode %d: %v", episode.Number, err)
		_ = active.session.ShowText(active.ctx, "Could not load episode "+fmt.Sprint(episode.Number))
		return
	}
	active.index = target
	active.stream = stream
	active.position = start
	active.duration = 0
	active.lastSaved = time.Now()
	active.subtitlePending = stream.Subtitle != ""
	active.complete = false
	s.logger.Printf("playback navigated media_id=%d episode=%d", episode.MediaID, episode.Number)
	_ = active.session.ShowText(active.ctx, fmt.Sprintf("Episode %d", episode.Number))
}

func (s *Service) resolveStream(ctx context.Context, episode Episode) (mpv.Stream, error) {
	streams, err := s.provider.Streams(ctx, allanime.Episode{
		ShowID: episode.ProviderID, Number: episode.Number, Value: fmt.Sprint(episode.Number),
	}, s.config.Translation, s.config.PreferredQuality)
	if err != nil {
		return mpv.Stream{}, fmt.Errorf("resolve AllAnime episode %d: %w", episode.Number, err)
	}
	if len(streams) == 0 {
		return mpv.Stream{}, fmt.Errorf("resolve AllAnime episode %d: no playable streams", episode.Number)
	}
	stream := streams[0]
	s.logger.Printf("stream resolved provider=allanime media_id=%d episode=%d", episode.MediaID, episode.Number)
	return mpv.Stream{URL: stream.URL, Headers: stream.Headers, Subtitle: stream.Subtitle}, nil
}

func (s *Service) availableEpisodes(ctx context.Context, mediaID int) ([]Episode, error) {
	s.cacheMu.RLock()
	episodes := append([]Episode(nil), s.episodes[mediaID]...)
	s.cacheMu.RUnlock()
	if len(episodes) > 0 {
		return episodes, nil
	}
	return s.Episodes(ctx, mediaID)
}

func (s *Service) resolveProvider(ctx context.Context, mediaID int) (anilist.Media, allanime.Anime, error) {
	s.cacheMu.RLock()
	media, ok := s.media[mediaID]
	s.cacheMu.RUnlock()
	if !ok {
		var err error
		media, err = s.anilist.Get(ctx, mediaID)
		if err != nil {
			return anilist.Media{}, allanime.Anime{}, err
		}
		s.cacheMu.Lock()
		s.media[mediaID] = media
		s.cacheMu.Unlock()
	}
	if s.state != nil {
		providerID, found, err := s.state.ProviderMapping(ctx, mediaID, "allanime")
		if err != nil {
			s.logger.Printf("load provider mapping media_id=%d: %v", mediaID, err)
		} else if found {
			return media, allanime.Anime{ID: providerID, AniListID: mediaID}, nil
		}
	}
	aliases := []string{media.English, media.Romaji, media.Native, media.Title}
	aliases = append(aliases, media.Synonyms...)
	match, err := s.provider.Match(ctx, mediaID, aliases, s.config.Translation)
	if err != nil {
		return anilist.Media{}, allanime.Anime{}, fmt.Errorf("match provider title for AniList %d: %w", mediaID, err)
	}
	s.logger.Printf("provider match provider=allanime media_id=%d provider_id=%s", mediaID, match.ID)
	if s.state != nil {
		if err := s.state.SaveProviderMapping(ctx, mediaID, "allanime", match.ID); err != nil {
			s.logger.Printf("save provider mapping media_id=%d: %v", mediaID, err)
		}
	}
	return media, match, nil
}

func (s *Service) resumePosition(ctx context.Context, episode Episode) float64 {
	if !s.config.Resume || s.state == nil {
		return 0
	}
	progress, found, err := s.state.Progress(ctx, episode.MediaID, episode.Number)
	if err != nil {
		s.logger.Printf("load watch progress result_id=%s: %v", episode.ResultID(), err)
		return 0
	}
	if !found || progress.Complete {
		return 0
	}
	return max(0, progress.Position-s.config.ResumeRewind)
}

func (s *Service) saveProgress(active *activePlayback, complete bool) {
	active.complete = complete
	if s.state == nil || active.index < 0 || active.index >= len(active.episodes) {
		return
	}
	episode := active.episodes[active.index]
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.state.SaveProgress(ctx, store.Progress{
		MediaID: episode.MediaID, Episode: episode.Number,
		Position: active.position, Duration: active.duration, Complete: complete,
	}); err != nil {
		s.logger.Printf("save watch progress result_id=%s: %v", episode.ResultID(), err)
		return
	}
	active.lastSaved = time.Now()
}

func episodeIndex(episodes []Episode, number int) int {
	for index, episode := range episodes {
		if episode.Number == number {
			return index
		}
	}
	return -1
}

func mediaFromAniList(item anilist.Media) Media {
	aliases := []string{item.English, item.Romaji, item.Native}
	aliases = append(aliases, item.Synonyms...)
	return Media{
		ID: item.ID, Title: item.Title, Format: item.Format, Episodes: item.Episodes,
		CoverURL: item.CoverURL, Aliases: aliases,
	}
}
