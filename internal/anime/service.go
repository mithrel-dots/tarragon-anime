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
	"tarragon-anime/internal/aniskip"
	"tarragon-anime/internal/mpv"
	"tarragon-anime/internal/provider"
	"tarragon-anime/internal/store"
)

type aniListClient interface {
	Search(context.Context, string) ([]anilist.Media, error)
	Get(context.Context, int) (anilist.Media, error)
}

type providerClient = provider.Client

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

type skipClient interface {
	SkipTimes(context.Context, int, int, float64) ([]aniskip.SkipTime, error)
}

// tokenStore exposes AniList sign-in state so sync activates as soon as a
// token is saved, without restarting the plugin.
type tokenStore interface {
	Token() (string, error)
	Save(string) error
	Delete() error
	AuthorizationURL(string) (string, error)
}

// browser opens the AniList consent page in the user's default browser.
type browser interface {
	Open(string) error
}

type Service struct {
	anilist   aniListClient
	providers map[string]providerClient
	player    player
	state     stateStore
	preview   previewCache
	sync      syncClient
	tokens    tokenStore
	browser   browser
	config    Config
	logger    *log.Logger
	skips     skipClient

	cacheMu  sync.RWMutex
	media    map[int]anilist.Media
	episodes map[int][]Episode

	playbackMu sync.Mutex
	active     *activePlayback

	syncMu sync.Mutex

	accountMu    sync.Mutex
	accountToken string
	accountName  string

	rootCtx      context.Context
	rootCancel   context.CancelFunc
	lifecycleMu  sync.Mutex
	closed       bool
	workers      sync.WaitGroup
	serviceClose sync.Once
}

type activePlayback struct {
	ctx      context.Context
	cancel   context.CancelFunc
	session  mpv.SessionController
	episodes []Episode
	index    int
	stream   mpv.Stream
	loaded   bool
	malID    int

	position        float64
	duration        float64
	lastSaved       time.Time
	subtitlePending bool
	complete        bool
	synced          bool
	skipTimes       []aniskip.SkipTime
	skipLoaded      bool
	skipped         map[int]bool
	nextResults     chan nextResult
	nextPrefetch    *nextPrefetch
	nextReady       *nextResult
	nextGeneration  uint64
	nextTriggered   bool
	ended           bool
	closeOnce       sync.Once
}

type nextPrefetch struct {
	generation uint64
	cancel     context.CancelFunc
}

type nextResult struct {
	generation uint64
	episode    Episode
	stream     mpv.Stream
	err        error
}

func NewService(anilistClient aniListClient, provider providerClient, player player, state stateStore, previews previewCache, syncer syncClient, config Config, logger *log.Logger) *Service {
	return NewServiceWithProviders(anilistClient, map[string]providerClient{"allanime": provider}, player, state, previews, syncer, config, logger)
}

func NewServiceWithProviders(anilistClient aniListClient, providers map[string]providerClient, player player, state stateStore, previews previewCache, syncer syncClient, config Config, logger *log.Logger) *Service {
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	rootCtx, rootCancel := context.WithCancel(context.Background())
	return &Service{
		anilist: anilistClient, providers: cloneProviders(providers), player: player, state: state, preview: previews,
		sync: syncer, config: config, logger: logger, media: make(map[int]anilist.Media),
		episodes: make(map[int][]Episode), rootCtx: rootCtx, rootCancel: rootCancel,
	}
}

func cloneProviders(values map[string]providerClient) map[string]providerClient {
	result := make(map[string]providerClient, len(values))
	for name, client := range values {
		result[strings.ToLower(strings.TrimSpace(name))] = client
	}
	return result
}

// WithAuth enables the AniList sign-in commands.
func (s *Service) WithAuth(tokens tokenStore, opener browser) *Service {
	s.tokens = tokens
	s.browser = opener
	return s
}

// WithSkip enables optional AniSkip metadata without making playback depend on
// the external service being available.
func (s *Service) WithSkip(client skipClient) *Service {
	s.skips = client
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
	if s.browser == nil || s.tokens == nil {
		return fmt.Errorf("AniList sign-in is unavailable")
	}
	url, err := s.tokens.AuthorizationURL(s.config.ClientID)
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
	media, err := s.mediaFor(ctx, mediaID)
	if err != nil {
		return nil, err
	}
	var failures []string
	for _, name := range s.providerNames() {
		if s.providers[name] == nil {
			failures = append(failures, name+": unavailable")
			continue
		}
		result, err := s.providerEpisodes(ctx, media, name)
		if err != nil {
			failures = append(failures, name+": "+err.Error())
			s.logger.Printf("provider episodes failed provider=%s media_id=%d: %v", name, mediaID, err)
			continue
		}
		s.logger.Printf("provider episodes provider=%s media_id=%d count=%d", name, mediaID, len(result))
		s.cacheMu.Lock()
		s.episodes[mediaID] = append([]Episode(nil), result...)
		s.cacheMu.Unlock()
		return result, nil
	}
	if len(failures) == 0 {
		return nil, fmt.Errorf("no providers configured for AniList %d", mediaID)
	}
	return nil, fmt.Errorf("resolve episodes for AniList %d: %s", mediaID, strings.Join(failures, "; "))
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
	if episode.Provider == "" || episode.ProviderID == "" {
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
	episode = episodes[index]
	stream, resolvedEpisode, err := s.resolveStream(ctx, episode)
	if err != nil {
		return err
	}
	episode = resolvedEpisode
	malID := s.mediaMALID(ctx, episode)
	start := s.resumePosition(ctx, episode)
	s.logger.Printf("stream served provider=%s media_id=%d episode=%d", episode.Provider, episode.MediaID, episode.Number)
	session, err := s.player.Play(ctx, stream, episode.Title, start)
	if err != nil {
		return fmt.Errorf("launch mpv: %w", err)
	}
	playbackCtx, cancel := context.WithCancel(s.rootCtx)
	active := &activePlayback{
		ctx: playbackCtx, cancel: cancel, session: session,
		episodes: episodes, index: index, stream: stream,
		position: start, lastSaved: time.Now(),
		skipped:     make(map[int]bool),
		malID:       malID,
		nextResults: make(chan nextResult, 1),
	}

	s.playbackMu.Lock()
	previous := s.active
	s.active = active
	s.playbackMu.Unlock()
	if !s.launch(func() { s.controlPlayback(active) }) {
		active.stop()
		s.playbackMu.Lock()
		if s.active == active {
			s.active = nil
		}
		s.playbackMu.Unlock()
		return fmt.Errorf("service is closed")
	}
	if previous != nil {
		previous.stop()
	}
	return nil
}

func (s *Service) Close() {
	s.serviceClose.Do(func() {
		s.lifecycleMu.Lock()
		s.closed = true
		s.rootCancel()
		s.lifecycleMu.Unlock()

		s.playbackMu.Lock()
		active := s.active
		s.active = nil
		s.playbackMu.Unlock()
		if active != nil {
			active.stop()
		}
		s.workers.Wait()
	})
}

func (s *Service) launch(run func()) bool {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.closed {
		return false
	}
	s.workers.Add(1)
	go func() {
		defer s.workers.Done()
		run()
	}()
	return true
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
		case result := <-active.nextResults:
			s.handleNextResult(active, result)
		}
	}
}

func (s *Service) handlePlaybackEvent(active *activePlayback, event mpv.Event) {
	switch event.Type {
	case mpv.EventPosition:
		active.position = event.Value
		s.autoSkip(active)
		thresholdReached := active.duration > 0 &&
			active.position/active.duration*100 >= s.config.Sync.ThresholdPercent
		autoNextReached := active.duration > 0 &&
			active.position/active.duration*100 >= s.config.AutoNextThresholdPercent
		if !active.complete && (thresholdReached || (s.config.AutoNext && autoNextReached)) {
			s.saveProgress(active, true)
		}
		if autoNextReached {
			s.startNextPrefetch(active)
		}
		if !active.complete && time.Since(active.lastSaved) >= 10*time.Second {
			s.saveProgress(active, false)
		}
		if s.config.Sync.Trigger == "threshold" && thresholdReached {
			s.queueSync(active)
		}
	case mpv.EventDuration:
		active.duration = event.Value
		if event.Value > 0 && !active.skipLoaded {
			active.skipLoaded = true
			active.skipTimes = s.fetchSkipTimes(active.ctx, active.malID, active.episodes[active.index], event.Value)
		}
	case mpv.EventFileLoaded:
		active.loaded = true
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
			if s.config.AutoNext {
				active.ended = true
				s.startNextPrefetch(active)
				s.loadPreparedNext(active)
			} else {
				active.stop()
			}
		}
	case mpv.EventNext:
		s.cancelNextPrefetch(active)
		s.navigate(active, 1, true)
	case mpv.EventPrevious:
		s.cancelNextPrefetch(active)
		s.navigate(active, -1, true)
	case mpv.EventSkip:
		s.manualSkip(active)
	case mpv.EventShutdown:
		active.cancel()
	}
}

func (s *Service) startNextPrefetch(active *activePlayback) {
	if !s.config.AutoNext || active.nextTriggered {
		return
	}
	active.nextTriggered = true
	targetIndex := active.index + 1
	if targetIndex >= len(active.episodes) {
		_ = active.session.ShowText(active.ctx, "No next episode available")
		return
	}
	target := active.episodes[targetIndex]
	active.nextGeneration++
	generation := active.nextGeneration
	ctx, cancel := context.WithCancel(active.ctx)
	active.nextPrefetch = &nextPrefetch{generation: generation, cancel: cancel}
	if !s.launch(func() {
		stream, resolved, err := s.resolveStream(ctx, target)
		select {
		case active.nextResults <- nextResult{generation: generation, episode: resolved, stream: stream, err: err}:
		case <-ctx.Done():
		}
	}) {
		cancel()
		active.nextPrefetch = nil
	}
}

func (s *Service) handleNextResult(active *activePlayback, result nextResult) {
	prefetch := active.nextPrefetch
	if prefetch == nil || result.generation != prefetch.generation {
		return
	}
	active.nextPrefetch = nil
	prefetch.cancel()
	if result.err != nil {
		s.logger.Printf("prefetch next episode %d: %v", result.episode.Number, result.err)
		_ = active.session.ShowText(active.ctx, "Could not prepare next episode")
		return
	}
	active.nextReady = &result
	if active.ended {
		s.loadPreparedNext(active)
	}
}

func (s *Service) loadPreparedNext(active *activePlayback) {
	if active.nextReady == nil {
		return
	}
	result := active.nextReady
	active.nextReady = nil
	start := s.resumePosition(active.ctx, result.episode)
	malID := s.mediaMALID(active.ctx, result.episode)
	if err := s.loadEpisode(active, result.episode, result.stream, start, malID); err != nil {
		s.logger.Printf("load next episode %d: %v", result.episode.Number, err)
		_ = active.session.ShowText(active.ctx, "Could not load next episode")
	}
}

func (s *Service) cancelNextPrefetch(active *activePlayback) {
	if active.nextPrefetch != nil {
		active.nextPrefetch.cancel()
		active.nextPrefetch = nil
	}
	active.nextReady = nil
	active.nextGeneration++
}

// Run exchanges AniList progress until the plugin shuts down, so remote history
// is imported and progress recorded while offline is not lost.
func (s *Service) Run(ctx context.Context) {
	if !s.syncConfigured() {
		return
	}
	runCtx, cancel := context.WithCancel(ctx)
	stopRootCancel := context.AfterFunc(s.rootCtx, cancel)
	defer func() {
		stopRootCancel()
		cancel()
	}()
	s.PullSync(runCtx)
	s.FlushSync(runCtx)
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-runCtx.Done():
			return
		case <-ticker.C:
			s.PullSync(runCtx)
			s.FlushSync(runCtx)
		}
	}
}

func (s *Service) syncConfigured() bool {
	return s.config.Sync.Enabled && s.sync != nil && s.state != nil
}

func (s *Service) syncEnabled() bool {
	if !s.syncConfigured() {
		return false
	}
	// Queued updates are retained until the user signs in.
	return s.tokens == nil || s.SignedIn()
}

// PullSync imports the signed-in user's AniList progress into local history.
func (s *Service) PullSync(ctx context.Context) {
	if !s.syncEnabled() {
		return
	}
	s.syncMu.Lock()
	defer s.syncMu.Unlock()

	viewer, err := s.sync.Viewer(ctx)
	if err != nil {
		s.logger.Printf("get AniList viewer for history sync: %v", err)
		return
	}
	items, err := s.sync.List(ctx, viewer.ID, "")
	if err != nil {
		s.logger.Printf("read AniList history: %v", err)
		return
	}
	for _, item := range items {
		if ctx.Err() != nil {
			return
		}
		if item.Progress <= 0 {
			continue
		}
		local, found, err := s.state.MediaProgress(ctx, item.Media.ID)
		if err != nil {
			s.logger.Printf("read local history media_id=%d: %v", item.Media.ID, err)
			continue
		}
		if found {
			switch s.config.Sync.Conflict {
			case "local":
				continue
			case "highest":
				if local.Episode >= item.Progress {
					continue
				}
			case "remote":
				if local.Episode == item.Progress && local.Complete {
					continue
				}
			}
		}
		s.cacheMedia(ctx, item.Media)
		if err := s.state.SaveProgress(ctx, store.Progress{
			MediaID: item.Media.ID, Episode: item.Progress, Complete: true,
		}); err != nil {
			s.logger.Printf("import AniList history media_id=%d episode=%d: %v", item.Media.ID, item.Progress, err)
			continue
		}
		s.logger.Printf("AniList history imported media_id=%d episode=%d", item.Media.ID, item.Progress)
	}
}

func (s *Service) queueSync(active *activePlayback) {
	if !s.syncEnabled() || active.synced || active.index < 0 || active.index >= len(active.episodes) {
		return
	}
	episode := active.episodes[active.index]
	ctx, cancel := context.WithTimeout(context.WithoutCancel(active.ctx), 5*time.Second)
	defer cancel()
	if err := s.state.EnqueueSync(ctx, episode.MediaID, episode.Number); err != nil {
		s.logger.Printf("queue AniList sync media_id=%d episode=%d: %v", episode.MediaID, episode.Number, err)
		return
	}
	active.synced = true
	s.logger.Printf("queued AniList sync media_id=%d episode=%d", episode.MediaID, episode.Number)
	s.launch(func() {
		flushCtx, cancel := context.WithTimeout(s.rootCtx, 30*time.Second)
		defer cancel()
		s.FlushSync(flushCtx)
	})
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
		if found {
			s.logger.Printf("AniList progress kept media_id=%d remote=%d local=%d", item.MediaID, remote.Progress, item.Episode)
			return nil
		}
	}
	status := "CURRENT"
	total, err := s.totalEpisodes(ctx, item.MediaID)
	if err != nil {
		return err
	}
	if total > 0 && target >= total {
		status = "COMPLETED"
	}
	if found && remote.Progress == target && (status != "COMPLETED" || remote.Status == "COMPLETED") {
		return nil
	}
	entry, err := s.sync.SaveProgress(ctx, item.MediaID, target, status)
	if err != nil {
		return err
	}
	s.logger.Printf("AniList progress synced media_id=%d episode=%d status=%s", item.MediaID, entry.Progress, entry.Status)
	return nil
}

func (s *Service) totalEpisodes(ctx context.Context, mediaID int) (int, error) {
	s.cacheMu.RLock()
	media, ok := s.media[mediaID]
	s.cacheMu.RUnlock()
	if ok {
		return media.Episodes, nil
	}
	media, err := s.anilist.Get(ctx, mediaID)
	if err != nil {
		return 0, fmt.Errorf("look up media %d for sync status: %w", mediaID, err)
	}
	s.cacheMu.Lock()
	s.media[mediaID] = media
	s.cacheMu.Unlock()
	return media.Episodes, nil
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
	stream, episode, err := s.resolveStream(active.ctx, episode)
	if err != nil {
		s.logger.Printf("navigate to episode %d: %v", episode.Number, err)
		_ = active.session.ShowText(active.ctx, "Could not resolve episode "+fmt.Sprint(episode.Number))
		return
	}
	malID := s.mediaMALID(active.ctx, episode)
	start := s.resumePosition(active.ctx, episode)
	if err := s.loadEpisode(active, episode, stream, start, malID); err != nil {
		s.logger.Printf("load episode %d: %v", episode.Number, err)
		_ = active.session.ShowText(active.ctx, "Could not load episode "+fmt.Sprint(episode.Number))
		return
	}
	s.logger.Printf("playback navigated media_id=%d episode=%d", episode.MediaID, episode.Number)
	_ = active.session.ShowText(active.ctx, fmt.Sprintf("Episode %d", episode.Number))
}

func (s *Service) loadEpisode(active *activePlayback, episode Episode, stream mpv.Stream, start float64, malID int) error {
	s.logger.Printf("stream served provider=%s media_id=%d episode=%d", episode.Provider, episode.MediaID, episode.Number)
	if err := active.session.Load(active.ctx, stream, episode.Title, start); err != nil {
		return err
	}
	active.index = episodeIndex(active.episodes, episode.Number)
	active.stream = stream
	active.position = start
	active.duration = 0
	active.loaded = false
	active.malID = malID
	active.lastSaved = time.Now()
	active.subtitlePending = stream.Subtitle != ""
	active.skipTimes = nil
	active.skipLoaded = false
	active.skipped = make(map[int]bool)
	active.complete = false
	active.synced = false
	active.nextTriggered = false
	active.ended = false
	active.nextGeneration++
	return nil
}

func (s *Service) mediaMALID(ctx context.Context, episode Episode) int {
	if !s.config.Skip.Enabled || s.skips == nil {
		return 0
	}
	s.cacheMu.RLock()
	media, found := s.media[episode.MediaID]
	s.cacheMu.RUnlock()
	if !found {
		if s.anilist == nil {
			return 0
		}
		var err error
		media, err = s.anilist.Get(ctx, episode.MediaID)
		if err != nil {
			s.logger.Printf("load AniSkip media media_id=%d: %v", episode.MediaID, err)
			return 0
		}
		s.cacheMu.Lock()
		s.media[episode.MediaID] = media
		s.cacheMu.Unlock()
	}
	return media.IDMal
}

func (s *Service) fetchSkipTimes(ctx context.Context, malID int, episode Episode, duration float64) []aniskip.SkipTime {
	if malID <= 0 || duration <= 0 {
		return nil
	}
	skipCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	times, err := s.skips.SkipTimes(skipCtx, malID, episode.Number, duration)
	if err != nil {
		s.logger.Printf("load AniSkip times media_id=%d episode=%d: %v", episode.MediaID, episode.Number, err)
		return nil
	}
	return times
}

func (s *Service) autoSkip(active *activePlayback) {
	if !active.loaded {
		return
	}
	for index, skip := range active.skipTimes {
		if active.skipped[index] || !s.skipTypeEnabled(skip.Type) {
			continue
		}
		start := max(0, skip.Start-s.config.Skip.MarginSeconds)
		if active.position < start || active.position >= skip.End {
			continue
		}
		if s.seekSkip(active, index, skip) {
			return
		}
	}
}

func (s *Service) manualSkip(active *activePlayback) {
	for index, skip := range active.skipTimes {
		if active.skipped[index] || !s.skipTypeEnabled(skip.Type) || active.position >= skip.End+s.config.Skip.MarginSeconds {
			continue
		}
		if s.seekSkip(active, index, skip) {
			return
		}
	}
	_ = active.session.ShowText(active.ctx, "No intro or outro to skip")
}

func (s *Service) seekSkip(active *activePlayback, index int, skip aniskip.SkipTime) bool {
	seeker, ok := active.session.(interface {
		Seek(context.Context, float64) error
	})
	if !ok {
		s.logger.Printf("skip unavailable: mpv session does not support seeking")
		return false
	}
	target := skip.End + s.config.Skip.MarginSeconds
	if err := seeker.Seek(active.ctx, target); err != nil {
		s.logger.Printf("skip %s at %.3f: %v", skip.Type, target, err)
		return false
	}
	if active.skipped == nil {
		active.skipped = make(map[int]bool)
	}
	active.position = target
	active.skipped[index] = true
	_ = active.session.ShowText(active.ctx, "Skipped "+skipLabel(skip.Type))
	return true
}

func (s *Service) skipTypeEnabled(kind string) bool {
	switch kind {
	case aniskip.Opening:
		return s.config.Skip.Intro
	case aniskip.Ending:
		return s.config.Skip.Outro
	default:
		return false
	}
}

func skipLabel(kind string) string {
	if kind == aniskip.Ending {
		return "outro"
	}
	return "intro"
}

func (s *Service) resolveStream(ctx context.Context, episode Episode) (mpv.Stream, Episode, error) {
	var failures []string
	for _, name := range s.providerNames() {
		if s.providers[name] == nil {
			continue
		}
		candidate := episode
		if name != episode.Provider {
			media, err := s.mediaFor(ctx, episode.MediaID)
			if err != nil {
				failures = append(failures, name+": load media: "+err.Error())
				continue
			}
			episodes, err := s.providerEpisodes(ctx, media, name)
			if err != nil {
				failures = append(failures, name+": "+err.Error())
				s.logger.Printf("provider stream fallback episodes failed provider=%s media_id=%d: %v", name, episode.MediaID, err)
				continue
			}
			found := false
			for _, item := range episodes {
				if item.Number == episode.Number {
					candidate, found = item, true
					break
				}
			}
			if !found {
				failures = append(failures, name+": episode is unavailable")
				continue
			}
		}
		client := s.providers[candidate.Provider]
		streams, err := client.Streams(ctx, provider.Episode{
			ShowID: candidate.ProviderID, Number: candidate.Number, Value: candidate.Value,
		}, s.config.Translation, s.config.PreferredQuality)
		if err != nil {
			failures = append(failures, candidate.Provider+": "+err.Error())
			continue
		}
		if len(streams) == 0 {
			failures = append(failures, candidate.Provider+": no playable streams")
			continue
		}
		stream := streams[0]
		return mpv.Stream{URL: stream.URL, Headers: stream.Headers, Subtitle: stream.Subtitle}, candidate, nil
	}
	if len(failures) == 0 {
		return mpv.Stream{}, episode, fmt.Errorf("no configured provider can serve episode %d", episode.Number)
	}
	return mpv.Stream{}, episode, fmt.Errorf("resolve episode %d stream: %s", episode.Number, strings.Join(failures, "; "))
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

func (s *Service) mediaFor(ctx context.Context, mediaID int) (anilist.Media, error) {
	s.cacheMu.RLock()
	media, ok := s.media[mediaID]
	s.cacheMu.RUnlock()
	if !ok {
		var err error
		media, err = s.anilist.Get(ctx, mediaID)
		if err != nil {
			return anilist.Media{}, err
		}
		s.cacheMu.Lock()
		s.media[mediaID] = media
		s.cacheMu.Unlock()
	}
	return media, nil
}

func (s *Service) providerNames() []string {
	configured := s.config.Providers
	if configured == nil && s.config.Provider != "" {
		configured = []string{s.config.Provider}
	}
	seen := make(map[string]bool, len(configured))
	result := make([]string, 0, len(configured))
	for _, name := range configured {
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		result = append(result, name)
	}
	return result
}

func (s *Service) resolveProvider(ctx context.Context, mediaID int, name string) (provider.Anime, error) {
	media, err := s.mediaFor(ctx, mediaID)
	if err != nil {
		return provider.Anime{}, err
	}
	client := s.providers[name]
	if client == nil {
		return provider.Anime{}, fmt.Errorf("provider %q is unavailable", name)
	}
	if s.state != nil {
		providerID, found, err := s.state.ProviderMapping(ctx, mediaID, name)
		if err != nil {
			s.logger.Printf("load provider mapping media_id=%d: %v", mediaID, err)
		} else if found {
			return provider.Anime{ID: providerID, AniListID: mediaID}, nil
		}
	}
	aliases := []string{media.English, media.Romaji, media.Native, media.Title}
	aliases = append(aliases, media.Synonyms...)
	match, err := client.Match(ctx, mediaID, aliases, s.config.Translation)
	if err != nil {
		return provider.Anime{}, fmt.Errorf("match %s title for AniList %d: %w", name, mediaID, err)
	}
	s.logger.Printf("provider match provider=%s media_id=%d provider_id=%s", name, mediaID, match.ID)
	if s.state != nil {
		if err := s.state.SaveProviderMapping(ctx, mediaID, name, match.ID); err != nil {
			s.logger.Printf("save provider mapping media_id=%d: %v", mediaID, err)
		}
	}
	return match, nil
}

func (s *Service) providerEpisodes(ctx context.Context, media anilist.Media, name string) ([]Episode, error) {
	providerAnime, err := s.resolveProvider(ctx, media.ID, name)
	if err != nil {
		return nil, err
	}
	items, err := s.providers[name].Episodes(ctx, providerAnime, s.config.Translation)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("provider returned no episodes")
	}
	result := make([]Episode, 0, len(items))
	for _, item := range items {
		if item.Number <= 0 {
			continue
		}
		result = append(result, Episode{
			MediaID: media.ID, Number: item.Number,
			Title:    fmt.Sprintf("%s - Episode %d", media.Title, item.Number),
			Provider: name, ProviderID: providerAnime.ID, Value: item.Value,
		})
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("provider returned no valid episodes")
	}
	return result, nil
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
	ctx, cancel := context.WithTimeout(context.WithoutCancel(active.ctx), 2*time.Second)
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
