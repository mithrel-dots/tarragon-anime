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
	"tarragon-anime/internal/auth"
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
	SaveMedia(context.Context, store.MediaInfo) error
	MediaProgress(context.Context, int) (store.Progress, bool, error)
	ResumeEntries(context.Context, int) ([]store.ResumeEntry, error)
	EnqueueSync(context.Context, int, int) error
	PendingSyncs(context.Context, int) ([]store.SyncItem, error)
	DeleteSync(context.Context, int64) error
	RecordSyncFailure(context.Context, int64, string) error
}

type syncClient interface {
	ListEntry(context.Context, int) (anilist.ListEntry, bool, error)
	SaveProgress(context.Context, int, int, string) (anilist.ListEntry, error)
	List(context.Context, int, string) ([]anilist.ListItem, error)
	SetStatus(context.Context, int, string) error
	Viewer(context.Context) (anilist.Viewer, error)
}

type previewCache interface {
	Get(context.Context, int, string) (string, error)
}

// tokenStore exposes AniList sign-in state so sync activates as soon as a
// token is saved, without restarting the plugin.
type tokenStore interface {
	Token() (string, error)
	Save(string) error
	Delete() error
}

// browser opens the AniList consent page in the user's default browser.
type browser interface {
	Open(string) error
}

type Service struct {
	anilist  aniListClient
	provider providerClient
	player   player
	state    stateStore
	preview  previewCache
	sync     syncClient
	tokens   tokenStore
	browser  browser
	config   Config
	logger   *log.Logger

	cacheMu  sync.RWMutex
	media    map[int]anilist.Media
	episodes map[int][]Episode

	playbackMu sync.Mutex
	active     *activePlayback

	syncMu sync.Mutex

	accountMu    sync.Mutex
	accountToken string
	accountName  string
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
	synced          bool
	closeOnce       sync.Once
}

func NewService(anilistClient aniListClient, provider providerClient, player player, state stateStore, previews previewCache, syncer syncClient, config Config, logger *log.Logger) *Service {
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	return &Service{
		anilist: anilistClient, provider: provider, player: player, state: state, preview: previews,
		sync: syncer, config: config, logger: logger, media: make(map[int]anilist.Media),
		episodes: make(map[int][]Episode),
	}
}

// WithAuth enables the AniList sign-in commands.
func (s *Service) WithAuth(tokens tokenStore, opener browser) *Service {
	s.tokens = tokens
	s.browser = opener
	return s
}

// SignedIn reports whether an AniList token is currently stored.
func (s *Service) SignedIn() bool {
	if s.tokens == nil {
		return false
	}
	token, err := s.tokens.Token()
	if err != nil {
		s.logger.Printf("read AniList token: %v", err)
		return false
	}
	return token != ""
}

// StartLogin opens the AniList consent page for the implicit grant.
func (s *Service) StartLogin(context.Context) error {
	if s.browser == nil {
		return fmt.Errorf("AniList sign-in is unavailable")
	}
	url, err := auth.AuthorizeURL(s.config.ClientID)
	if err != nil {
		return err
	}
	if err := s.browser.Open(url); err != nil {
		return fmt.Errorf("open AniList sign-in: %w", err)
	}
	s.logger.Printf("AniList sign-in started")
	return nil
}

// Account returns the signed-in AniList username, caching it per token so the
// launcher does not query AniList on every keystroke.
func (s *Service) Account(ctx context.Context) string {
	if s.tokens == nil || s.sync == nil {
		return ""
	}
	token, err := s.tokens.Token()
	if err != nil || token == "" {
		return ""
	}
	s.accountMu.Lock()
	defer s.accountMu.Unlock()
	if s.accountToken == token {
		return s.accountName
	}
	viewer, err := s.sync.Viewer(ctx)
	if err != nil {
		s.logger.Printf("look up AniList account: %v", err)
		return ""
	}
	s.accountToken, s.accountName = token, viewer.Name
	return s.accountName
}

// Logout removes the stored AniList token.
func (s *Service) Logout(context.Context) error {
	if s.tokens == nil {
		return fmt.Errorf("AniList sign-in is unavailable")
	}
	if err := s.tokens.Delete(); err != nil {
		return err
	}
	s.accountMu.Lock()
	s.accountToken, s.accountName = "", ""
	s.accountMu.Unlock()
	s.logger.Printf("AniList signed out")
	return nil
}

func (s *Service) List(ctx context.Context, status string) ([]ListItem, error) {
	if s.sync == nil {
		return nil, fmt.Errorf("AniList list access is unavailable")
	}
	viewer, err := s.sync.Viewer(ctx)
	if err != nil {
		return nil, fmt.Errorf("get AniList viewer for list: %w", err)
	}
	items, err := s.sync.List(ctx, viewer.ID, status)
	if err != nil {
		return nil, err
	}
	result := make([]ListItem, 0, len(items))
	for _, item := range items {
		media := s.cacheMedia(ctx, item.Media)
		result = append(result, ListItem{Media: media, Status: item.Status, Progress: item.Progress})
	}
	return result, nil
}

func (s *Service) SetListStatus(ctx context.Context, mediaID int, status string) error {
	if s.sync == nil {
		return fmt.Errorf("AniList list access is unavailable")
	}
	if err := s.sync.SetStatus(ctx, mediaID, status); err != nil {
		return err
	}
	return nil
}

func (s *Service) OpenMedia(_ context.Context, mediaID int) error {
	if s.browser == nil {
		return fmt.Errorf("AniList page opening is unavailable")
	}
	if err := s.browser.Open(fmt.Sprintf("https://anilist.co/anime/%d", mediaID)); err != nil {
		return fmt.Errorf("open AniList page: %w", err)
	}
	return nil
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
	for _, item := range items {
		result = append(result, s.cacheMedia(ctx, item))
	}
	return result, nil
}

func (s *Service) cacheMedia(ctx context.Context, item anilist.Media) Media {
	s.cacheMu.Lock()
	s.media[item.ID] = item
	s.cacheMu.Unlock()
	media := mediaFromAniList(item)
	if s.preview != nil && item.CoverURL != "" {
		path, err := s.preview.Get(ctx, item.ID, item.CoverURL)
		if err != nil {
			s.logger.Printf("cache preview media_id=%d: %v", item.ID, err)
		} else {
			media.CoverURL = path
		}
	}
	if s.state != nil {
		if err := s.state.SaveMedia(ctx, store.MediaInfo{
			ID: media.ID, Title: media.Title, PreviewPath: media.CoverURL, Episodes: media.Episodes,
		}); err != nil {
			s.logger.Printf("save media media_id=%d: %v", media.ID, err)
		}
	}
	return media
}

// ContinueWatching lists recently played anime so the launcher can resume without a search.
func (s *Service) ContinueWatching(ctx context.Context, limit int) ([]Resume, error) {
	if s.state == nil {
		return nil, nil
	}
	entries, err := s.state.ResumeEntries(ctx, limit)
	if err != nil {
		return nil, err
	}
	result := make([]Resume, 0, len(entries))
	for _, entry := range entries {
		episode := entry.Episode
		if entry.Complete && (entry.TotalEpisodes == 0 || episode < entry.TotalEpisodes) {
			episode++
		}
		result = append(result, Resume{
			MediaID: entry.MediaID, Title: entry.Title, PreviewPath: entry.PreviewPath,
			Episode: episode, Position: entry.Position, TotalEpisodes: entry.TotalEpisodes,
			Continues: !entry.Complete && entry.Position > 0,
		})
	}
	return result, nil
}

// ResumeMedia continues an anime from the last watched episode.
func (s *Service) ResumeMedia(ctx context.Context, mediaID int) error {
	episodes, err := s.availableEpisodes(ctx, mediaID)
	if err != nil {
		return err
	}
	if len(episodes) == 0 {
		return fmt.Errorf("no episodes are available for AniList %d", mediaID)
	}
	target := episodes[0]
	if s.state != nil {
		progress, found, err := s.state.MediaProgress(ctx, mediaID)
		if err != nil {
			s.logger.Printf("load media progress media_id=%d: %v", mediaID, err)
		} else if found {
			number := progress.Episode
			if progress.Complete {
				number++
			}
			if index := episodeIndex(episodes, number); index >= 0 {
				target = episodes[index]
			} else if index := episodeIndex(episodes, progress.Episode); index >= 0 {
				target = episodes[index]
			}
		}
	}
	return s.PlayEpisode(ctx, target)
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
		thresholdReached := active.duration > 0 &&
			active.position/active.duration*100 >= s.config.Sync.ThresholdPercent
		if !active.complete && thresholdReached {
			s.saveProgress(active, true)
		}
		if !active.complete && time.Since(active.lastSaved) >= 10*time.Second {
			s.saveProgress(active, false)
		}
		if s.config.Sync.Trigger == "threshold" && thresholdReached {
			s.queueSync(active)
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
		if s.config.Sync.Trigger == "start" {
			s.queueSync(active)
		}
	case mpv.EventEndFile:
		complete := event.Reason == "eof" || active.complete
		s.saveProgress(active, complete)
		if complete && s.config.Sync.Trigger == "eof" {
			s.queueSync(active)
		}
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

// Run flushes queued AniList updates until the plugin shuts down, so progress
// recorded while offline is not lost.
func (s *Service) Run(ctx context.Context) {
	if !s.syncEnabled() {
		return
	}
	s.FlushSync(ctx)
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.FlushSync(ctx)
		}
	}
}

func (s *Service) syncEnabled() bool {
	if !s.config.Sync.Enabled || s.sync == nil || s.state == nil {
		return false
	}
	// Queued updates are retained until the user signs in.
	return s.tokens == nil || s.SignedIn()
}

func (s *Service) queueSync(active *activePlayback) {
	if !s.syncEnabled() || active.synced || active.index < 0 || active.index >= len(active.episodes) {
		return
	}
	active.synced = true
	episode := active.episodes[active.index]
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.state.EnqueueSync(ctx, episode.MediaID, episode.Number); err != nil {
		s.logger.Printf("queue AniList sync media_id=%d episode=%d: %v", episode.MediaID, episode.Number, err)
		return
	}
	s.logger.Printf("queued AniList sync media_id=%d episode=%d", episode.MediaID, episode.Number)
	go func() {
		flushCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		s.FlushSync(flushCtx)
	}()
}

func (s *Service) FlushSync(ctx context.Context) {
	if !s.syncEnabled() {
		return
	}
	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	items, err := s.state.PendingSyncs(ctx, 32)
	if err != nil {
		s.logger.Printf("read AniList sync queue: %v", err)
		return
	}
	for _, item := range items {
		if ctx.Err() != nil {
			return
		}
		if err := s.pushSync(ctx, item); err != nil {
			s.logger.Printf("sync AniList media_id=%d episode=%d: %v", item.MediaID, item.Episode, err)
			if err := s.state.RecordSyncFailure(ctx, item.ID, err.Error()); err != nil {
				s.logger.Printf("record AniList sync failure: %v", err)
			}
			continue
		}
		if err := s.state.DeleteSync(ctx, item.ID); err != nil {
			s.logger.Printf("clear AniList sync item: %v", err)
		}
	}
}

func (s *Service) pushSync(ctx context.Context, item store.SyncItem) error {
	remote, found, err := s.sync.ListEntry(ctx, item.MediaID)
	if err != nil {
		return err
	}
	target := item.Episode
	switch s.config.Sync.Conflict {
	case "highest":
		if found && remote.Progress > target {
			target = remote.Progress
		}
	case "remote":
		if found && remote.Progress >= item.Episode {
			s.logger.Printf("AniList progress kept media_id=%d remote=%d local=%d", item.MediaID, remote.Progress, item.Episode)
			return nil
		}
	}
	if found && remote.Progress == target {
		return nil
	}
	status := "CURRENT"
	if total := s.totalEpisodes(ctx, item.MediaID); total > 0 && target >= total {
		status = "COMPLETED"
	}
	entry, err := s.sync.SaveProgress(ctx, item.MediaID, target, status)
	if err != nil {
		return err
	}
	s.logger.Printf("AniList progress synced media_id=%d episode=%d status=%s", item.MediaID, entry.Progress, entry.Status)
	return nil
}

func (s *Service) totalEpisodes(ctx context.Context, mediaID int) int {
	s.cacheMu.RLock()
	media, ok := s.media[mediaID]
	s.cacheMu.RUnlock()
	if ok {
		return media.Episodes
	}
	media, err := s.anilist.Get(ctx, mediaID)
	if err != nil {
		s.logger.Printf("look up media %d for sync status: %v", mediaID, err)
		return 0
	}
	s.cacheMu.Lock()
	s.media[mediaID] = media
	s.cacheMu.Unlock()
	return media.Episodes
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
	active.synced = false
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
