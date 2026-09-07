package anime

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"tarragon-anime/internal/aniskip"
	"tarragon-anime/internal/mpv"
	"tarragon-anime/internal/provider/allanime"
	"tarragon-anime/internal/store"
)

type navigationProvider struct{}

func (navigationProvider) Match(_ context.Context, id int, _ []string, _ string) (allanime.Anime, error) {
	return allanime.Anime{ID: "show", Name: "Frieren", AniListID: id}, nil
}

func (navigationProvider) Episodes(context.Context, allanime.Anime, string) ([]allanime.Episode, error) {
	return []allanime.Episode{
		{ShowID: "show", Number: 1, Value: "1"},
		{ShowID: "show", Number: 2, Value: "2"},
		{ShowID: "show", Number: 3, Value: "3"},
	}, nil
}

func (navigationProvider) Streams(_ context.Context, episode allanime.Episode, _, _ string) ([]allanime.Stream, error) {
	return []allanime.Stream{{URL: fmt.Sprintf("https://video.test/%d.m3u8", episode.Number)}}, nil
}

type autoNextProvider struct {
	mu          sync.Mutex
	episodes    []int
	streamCalls []int
	errors      map[int]error
	blockFirst  chan struct{}
	started     chan struct{}
	canceled    chan struct{}
	finish      chan struct{}
}

func (p *autoNextProvider) Match(_ context.Context, id int, _ []string, _ string) (allanime.Anime, error) {
	return allanime.Anime{ID: "show", Name: "Frieren", AniListID: id}, nil
}

func (p *autoNextProvider) Episodes(context.Context, allanime.Anime, string) ([]allanime.Episode, error) {
	result := make([]allanime.Episode, 0, len(p.episodes))
	for _, number := range p.episodes {
		result = append(result, allanime.Episode{ShowID: "show", Number: number, Value: fmt.Sprint(number)})
	}
	return result, nil
}

func (p *autoNextProvider) Streams(ctx context.Context, episode allanime.Episode, _, _ string) ([]allanime.Stream, error) {
	p.mu.Lock()
	p.streamCalls = append(p.streamCalls, episode.Number)
	firstBlocked := episode.Number == 2 && len(p.streamCalls) == 2 && p.blockFirst != nil
	p.mu.Unlock()
	if firstBlocked {
		close(p.started)
		select {
		case <-p.blockFirst:
		case <-ctx.Done():
			if p.canceled != nil {
				close(p.canceled)
			}
			if p.finish != nil {
				<-p.finish
			}
			return nil, ctx.Err()
		}
	}
	if err := p.errors[episode.Number]; err != nil {
		return nil, err
	}
	return []allanime.Stream{{
		URL:      fmt.Sprintf("https://video.test/%d.m3u8", episode.Number),
		Subtitle: fmt.Sprintf("https://sub.test/%d.ass", episode.Number),
	}}, nil
}

func (p *autoNextProvider) callsFor(number int) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	count := 0
	for _, call := range p.streamCalls {
		if call == number {
			count++
		}
	}
	return count
}

type loadCall struct {
	stream mpv.Stream
	title  string
	start  float64
}

type fakeSession struct {
	events chan mpv.Event

	mu        sync.Mutex
	loads     []loadCall
	seeks     []float64
	subtitles []string
	messages  []string
	closed    bool
	closedCh  chan struct{}
}

func newFakeSession() *fakeSession {
	return &fakeSession{events: make(chan mpv.Event, 16), closedCh: make(chan struct{})}
}

func (s *fakeSession) Events() <-chan mpv.Event { return s.events }

func (s *fakeSession) Load(_ context.Context, stream mpv.Stream, title string, start float64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loads = append(s.loads, loadCall{stream: stream, title: title, start: start})
	return nil
}

func (s *fakeSession) AddSubtitle(_ context.Context, subtitle string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subtitles = append(s.subtitles, subtitle)
	return nil
}

func (s *fakeSession) Seek(_ context.Context, position float64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seeks = append(s.seeks, position)
	return nil
}

func (s *fakeSession) ShowText(_ context.Context, message string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages = append(s.messages, message)
	return nil
}

func (s *fakeSession) Close() {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		close(s.closedCh)
	}
	s.mu.Unlock()
}

func (s *fakeSession) loadCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.loads)
}

func (s *fakeSession) lastLoad() loadCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loads[len(s.loads)-1]
}

func (s *fakeSession) seekCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.seeks)
}

func (s *fakeSession) lastSeek() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seeks[len(s.seeks)-1]
}

func (s *fakeSession) messagesCopy() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.messages...)
}

func (s *fakeSession) subtitleCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.subtitles)
}

func (s *fakeSession) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

type fakeSkipClient struct {
	times []aniskip.SkipTime
}

func (c fakeSkipClient) SkipTimes(context.Context, int, int, float64) ([]aniskip.SkipTime, error) {
	return c.times, nil
}

type fakePlayer struct {
	session *fakeSession
	start   float64
	plays   int
}

func (p *fakePlayer) Play(_ context.Context, _ mpv.Stream, _ string, start float64) (mpv.SessionController, error) {
	p.start = start
	p.plays++
	return p.session, nil
}

type memoryState struct {
	mu       sync.Mutex
	mapping  string
	progress map[[2]int]store.Progress
	media    map[int]store.MediaInfo
	queue    []store.SyncItem
	nextID   int64
	failures []string
	enqueue  error
}

func newMemoryState() *memoryState {
	return &memoryState{
		progress: make(map[[2]int]store.Progress),
		media:    make(map[int]store.MediaInfo),
	}
}

func (s *memoryState) SaveMedia(_ context.Context, media store.MediaInfo) error {
	s.mu.Lock()
	s.media[media.ID] = media
	s.mu.Unlock()
	return nil
}

func (s *memoryState) CachedMedia(_ context.Context, mediaID int) (store.MediaInfo, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	media, found := s.media[mediaID]
	return media, found, nil
}

func (s *memoryState) SearchMedia(_ context.Context, query string) ([]store.MediaInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	query = strings.ToLower(query)
	var result []store.MediaInfo
	for _, media := range s.media {
		if strings.Contains(strings.ToLower(media.Title), query) {
			result = append(result, media)
		}
	}
	return result, nil
}

func (s *memoryState) MediaProgress(_ context.Context, mediaID int) (store.Progress, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var latest store.Progress
	found := false
	for key, progress := range s.progress {
		if key[0] == mediaID && (!found || progress.Episode > latest.Episode) {
			latest, found = progress, true
		}
	}
	return latest, found, nil
}

func (s *memoryState) ResumeEntries(_ context.Context, _ int) ([]store.ResumeEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var entries []store.ResumeEntry
	for key, progress := range s.progress {
		media := s.media[key[0]]
		entries = append(entries, store.ResumeEntry{
			MediaID: key[0], Title: media.Title, TotalEpisodes: media.Episodes,
			Episode: progress.Episode, Position: progress.Position, Complete: progress.Complete,
		})
	}
	return entries, nil
}

func (s *memoryState) EnqueueSync(_ context.Context, mediaID, episode int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.enqueue != nil {
		return s.enqueue
	}
	for _, item := range s.queue {
		if item.MediaID == mediaID && item.Episode == episode {
			return nil
		}
	}
	s.nextID++
	s.queue = append(s.queue, store.SyncItem{ID: s.nextID, MediaID: mediaID, Episode: episode})
	return nil
}

func (s *memoryState) PendingSyncs(_ context.Context, _ int) ([]store.SyncItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]store.SyncItem(nil), s.queue...), nil
}

func (s *memoryState) DeleteSync(_ context.Context, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	remaining := s.queue[:0]
	for _, item := range s.queue {
		if item.ID != id {
			remaining = append(remaining, item)
		}
	}
	s.queue = remaining
	return nil
}

func (s *memoryState) RecordSyncFailure(_ context.Context, _ int64, reason string) error {
	s.mu.Lock()
	s.failures = append(s.failures, reason)
	s.mu.Unlock()
	return nil
}

func (s *memoryState) pendingCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.queue)
}

func (s *memoryState) ProviderMapping(context.Context, int, string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mapping, s.mapping != "", nil
}

func (s *memoryState) SaveProviderMapping(_ context.Context, _ int, _, providerID string) error {
	s.mu.Lock()
	s.mapping = providerID
	s.mu.Unlock()
	return nil
}

func (s *memoryState) Progress(_ context.Context, mediaID, episode int) (store.Progress, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	progress, ok := s.progress[[2]int{mediaID, episode}]
	return progress, ok, nil
}

func (s *memoryState) SaveProgress(_ context.Context, progress store.Progress) error {
	s.mu.Lock()
	s.progress[[2]int{progress.MediaID, progress.Episode}] = progress
	s.mu.Unlock()
	return nil
}

func (s *memoryState) saved(mediaID, episode int) store.Progress {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.progress[[2]int{mediaID, episode}]
}

type blockingSaveState struct {
	*memoryState
	started chan struct{}
	release chan struct{}
}

func (s *blockingSaveState) SaveProgress(ctx context.Context, progress store.Progress) error {
	select {
	case s.started <- struct{}{}:
	default:
	}
	<-s.release
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.memoryState.SaveProgress(ctx, progress)
}

func TestCloseWaitsForFinalProgressSave(t *testing.T) {
	session := newFakeSession()
	state := &blockingSaveState{
		memoryState: newMemoryState(),
		started:     make(chan struct{}, 1),
		release:     make(chan struct{}),
	}
	config := DefaultConfig()
	config.AutoNext = false
	service := NewService(&cachingAniList{}, navigationProvider{}, &fakePlayer{session: session}, state, nil, nil, config, nil)
	startTestPlayback(t, service)

	session.events <- mpv.Event{Type: mpv.EventDuration, Value: 100}
	session.events <- mpv.Event{Type: mpv.EventPosition, Value: 40}
	session.events <- mpv.Event{Type: mpv.EventSkip}
	waitFor(t, func() bool { return len(session.messagesCopy()) == 1 })

	closed := make(chan struct{})
	go func() {
		service.Close()
		close(closed)
	}()
	select {
	case <-state.started:
	case <-time.After(time.Second):
		t.Fatal("final progress save did not start")
	}
	select {
	case <-closed:
		t.Fatal("Close returned before final progress was persisted")
	default:
	}
	close(state.release)
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close did not return after final progress was persisted")
	}
	if progress := state.saved(154587, 1); progress.Position != 40 || progress.Duration != 100 || progress.Complete {
		t.Fatalf("final progress = %#v", progress)
	}
}

func TestPlaybackAutoNextAndPrevious(t *testing.T) {
	client := &cachingAniList{}
	session := newFakeSession()
	player := &fakePlayer{session: session}
	state := newMemoryState()
	state.progress[[2]int{154587, 1}] = store.Progress{MediaID: 154587, Episode: 1, Position: 42}
	config := DefaultConfig()
	config.ResumeRewind = 5
	service := NewService(client, navigationProvider{}, player, state, nil, nil, config, nil)
	defer service.Close()
	if _, err := service.Search(t.Context(), "frieren"); err != nil {
		t.Fatal(err)
	}
	episodes, err := service.Episodes(t.Context(), 154587)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.PlayEpisode(t.Context(), episodes[0]); err != nil {
		t.Fatal(err)
	}
	if player.start != 37 {
		t.Fatalf("initial resume position = %v, want 37", player.start)
	}

	session.events <- mpv.Event{Type: mpv.EventPosition, Value: 100}
	session.events <- mpv.Event{Type: mpv.EventDuration, Value: 1400}
	session.events <- mpv.Event{Type: mpv.EventEndFile, Reason: "eof"}
	waitFor(t, func() bool { return session.loadCount() == 1 })
	if call := session.lastLoad(); call.stream.URL != "https://video.test/2.m3u8" || call.start != 0 {
		t.Fatalf("auto-next load = %#v", call)
	}
	if progress := state.saved(154587, 1); !progress.Complete || progress.Position != 100 || progress.Duration != 1400 {
		t.Fatalf("completed progress = %#v", progress)
	}

	session.events <- mpv.Event{Type: mpv.EventPrevious}
	waitFor(t, func() bool { return session.loadCount() == 2 })
	if call := session.lastLoad(); call.stream.URL != "https://video.test/1.m3u8" || call.start != 0 {
		t.Fatalf("previous load = %#v", call)
	}
}

func TestPlaybackCanDisableAutoNextAndResume(t *testing.T) {
	client := &cachingAniList{}
	session := newFakeSession()
	player := &fakePlayer{session: session}
	state := newMemoryState()
	state.progress[[2]int{154587, 1}] = store.Progress{MediaID: 154587, Episode: 1, Position: 42}
	config := DefaultConfig()
	config.AutoNext = false
	config.Resume = false
	service := NewService(client, navigationProvider{}, player, state, nil, nil, config, nil)
	defer service.Close()
	if _, err := service.Search(t.Context(), "frieren"); err != nil {
		t.Fatal(err)
	}
	episodes, err := service.Episodes(t.Context(), 154587)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.PlayEpisode(t.Context(), episodes[0]); err != nil {
		t.Fatal(err)
	}
	if player.start != 0 {
		t.Fatalf("initial resume position = %v, want 0", player.start)
	}
	session.events <- mpv.Event{Type: mpv.EventEndFile, Reason: "eof"}
	select {
	case <-session.closedCh:
	case <-time.After(time.Second):
		t.Fatal("playback did not stop after end of file")
	}
	if session.loadCount() != 0 {
		t.Fatalf("load count = %d, want 0", session.loadCount())
	}
}

func TestPlaybackThresholdMarksEpisodeComplete(t *testing.T) {
	client := &cachingAniList{}
	session := newFakeSession()
	player := &fakePlayer{session: session}
	state := newMemoryState()
	config := DefaultConfig()
	config.AutoNext = false
	config.Sync.ThresholdPercent = 90
	service := NewService(client, navigationProvider{}, player, state, nil, nil, config, nil)
	defer service.Close()

	episodes, err := service.Episodes(t.Context(), 154587)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.PlayEpisode(t.Context(), episodes[0]); err != nil {
		t.Fatal(err)
	}
	session.events <- mpv.Event{Type: mpv.EventDuration, Value: 100}
	session.events <- mpv.Event{Type: mpv.EventPosition, Value: 90}
	waitFor(t, func() bool { return state.saved(154587, 1).Complete })
	if progress := state.saved(154587, 1); progress.Position != 90 || progress.Duration != 100 {
		t.Fatalf("threshold progress = %#v", progress)
	}
}

func TestPlaybackAutomaticallySkipsIntroAndManualSkipsOutro(t *testing.T) {
	client := &cachingAniList{}
	session := newFakeSession()
	player := &fakePlayer{session: session}
	config := DefaultConfig()
	config.AutoNext = false
	service := NewService(client, navigationProvider{}, player, nil, nil, nil, config, nil).
		WithSkip(fakeSkipClient{times: []aniskip.SkipTime{
			{Type: aniskip.Opening, Start: 5, End: 20},
			{Type: aniskip.Ending, Start: 80, End: 100},
		}})
	defer service.Close()
	if _, err := service.Search(t.Context(), "frieren"); err != nil {
		t.Fatal(err)
	}
	episodes, err := service.Episodes(t.Context(), 154587)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.PlayEpisode(t.Context(), episodes[0]); err != nil {
		t.Fatal(err)
	}
	session.events <- mpv.Event{Type: mpv.EventFileLoaded}
	session.events <- mpv.Event{Type: mpv.EventDuration, Value: 140}
	session.events <- mpv.Event{Type: mpv.EventPosition, Value: 6}
	waitFor(t, func() bool { return session.seekCount() == 1 })
	if got := session.lastSeek(); got != 20 {
		t.Fatalf("automatic intro seek = %v, want 20", got)
	}
	session.events <- mpv.Event{Type: mpv.EventPosition, Value: 50}
	session.events <- mpv.Event{Type: mpv.EventSkip}
	waitFor(t, func() bool { return session.seekCount() == 2 })
	if got := session.lastSeek(); got != 100 {
		t.Fatalf("manual outro seek = %v, want 100", got)
	}
}

func TestPlaybackAutoNextPrefetchesAndReusesSession(t *testing.T) {
	client := &cachingAniList{}
	session := newFakeSession()
	player := &fakePlayer{session: session}
	provider := &autoNextProvider{episodes: []int{1, 2, 3}}
	config := DefaultConfig()
	config.AutoNextThresholdPercent = 80
	service := NewService(client, provider, player, nil, nil, nil, config, nil)
	defer service.Close()

	if _, err := service.Search(t.Context(), "frieren"); err != nil {
		t.Fatal(err)
	}
	episodes, err := service.Episodes(t.Context(), 154587)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.PlayEpisode(t.Context(), episodes[0]); err != nil {
		t.Fatal(err)
	}
	session.events <- mpv.Event{Type: mpv.EventDuration, Value: 100}
	session.events <- mpv.Event{Type: mpv.EventPosition, Value: 80}
	waitFor(t, func() bool { return provider.callsFor(2) == 1 })
	if session.loadCount() != 0 {
		t.Fatalf("load count before current episode ends = %d, want 0", session.loadCount())
	}
	session.events <- mpv.Event{Type: mpv.EventEndFile, Reason: "eof"}
	waitFor(t, func() bool { return session.loadCount() == 1 })
	if player.plays != 1 {
		t.Fatalf("mpv Play calls = %d, want 1", player.plays)
	}
	call := session.lastLoad()
	if call.stream.URL != "https://video.test/2.m3u8" || call.title != "Frieren - Episode 2" || call.start != 0 {
		t.Fatalf("auto-next load = %#v", call)
	}
	session.events <- mpv.Event{Type: mpv.EventFileLoaded}
	waitFor(t, func() bool { return session.subtitleCount() == 1 })
	if provider.callsFor(2) != 1 || session.isClosed() {
		t.Fatalf("provider calls for episode 2 = %d, closed = %v", provider.callsFor(2), session.isClosed())
	}
}

func TestPlaybackAutoNextFailureLeavesMPVOpen(t *testing.T) {
	client := &cachingAniList{}
	session := newFakeSession()
	provider := &autoNextProvider{
		episodes: []int{1, 2},
		errors:   map[int]error{2: fmt.Errorf("stream unavailable")},
	}
	config := DefaultConfig()
	config.AutoNextThresholdPercent = 80
	service := NewService(client, provider, &fakePlayer{session: session}, nil, nil, nil, config, nil)
	defer service.Close()
	startTestPlayback(t, service)

	session.events <- mpv.Event{Type: mpv.EventDuration, Value: 100}
	session.events <- mpv.Event{Type: mpv.EventPosition, Value: 80}
	waitFor(t, func() bool { return len(session.messagesCopy()) > 0 })
	if got := session.messagesCopy()[0]; got != "Could not prepare next episode" {
		t.Fatalf("failure message = %q", got)
	}
	if session.loadCount() != 0 || session.isClosed() || provider.callsFor(2) != 1 {
		t.Fatalf("loads = %d, closed = %v, provider calls for episode 2 = %d", session.loadCount(), session.isClosed(), provider.callsFor(2))
	}
}

func TestPlaybackAutoNextLastEpisodeLeavesMPVOpen(t *testing.T) {
	client := &cachingAniList{}
	session := newFakeSession()
	provider := &autoNextProvider{episodes: []int{1}}
	config := DefaultConfig()
	config.AutoNextThresholdPercent = 80
	service := NewService(client, provider, &fakePlayer{session: session}, nil, nil, nil, config, nil)
	defer service.Close()
	startTestPlayback(t, service)

	session.events <- mpv.Event{Type: mpv.EventDuration, Value: 100}
	session.events <- mpv.Event{Type: mpv.EventPosition, Value: 80}
	waitFor(t, func() bool { return len(session.messagesCopy()) > 0 })
	if got := session.messagesCopy()[0]; got != "No next episode available" {
		t.Fatalf("last episode message = %q", got)
	}
	if session.loadCount() != 0 || session.isClosed() {
		t.Fatalf("loads = %d, closed = %v", session.loadCount(), session.isClosed())
	}
}

func TestPlaybackManualNavigationCancelsAutoNextPrefetch(t *testing.T) {
	client := &cachingAniList{}
	session := newFakeSession()
	provider := &autoNextProvider{
		episodes:   []int{1, 2, 3},
		blockFirst: make(chan struct{}),
		started:    make(chan struct{}),
	}
	config := DefaultConfig()
	config.AutoNextThresholdPercent = 80
	service := NewService(client, provider, &fakePlayer{session: session}, nil, nil, nil, config, nil)
	defer service.Close()
	startTestPlayback(t, service)

	session.events <- mpv.Event{Type: mpv.EventDuration, Value: 100}
	session.events <- mpv.Event{Type: mpv.EventPosition, Value: 80}
	select {
	case <-provider.started:
	case <-time.After(time.Second):
		t.Fatal("auto-next prefetch did not start")
	}
	session.events <- mpv.Event{Type: mpv.EventNext}
	waitFor(t, func() bool { return session.loadCount() == 1 })
	call := session.lastLoad()
	if call.stream.URL != "https://video.test/2.m3u8" {
		t.Fatalf("manual navigation load = %#v", call)
	}
	if provider.callsFor(2) != 2 || session.isClosed() {
		t.Fatalf("provider calls for episode 2 = %d, closed = %v", provider.callsFor(2), session.isClosed())
	}
}

func TestCloseCancelsAndWaitsForPrefetch(t *testing.T) {
	session := newFakeSession()
	provider := &autoNextProvider{
		episodes:   []int{1, 2},
		blockFirst: make(chan struct{}),
		started:    make(chan struct{}),
		canceled:   make(chan struct{}),
		finish:     make(chan struct{}),
	}
	config := DefaultConfig()
	config.AutoNextThresholdPercent = 80
	service := NewService(&cachingAniList{}, provider, &fakePlayer{session: session}, nil, nil, nil, config, nil)
	startTestPlayback(t, service)
	session.events <- mpv.Event{Type: mpv.EventDuration, Value: 100}
	session.events <- mpv.Event{Type: mpv.EventPosition, Value: 80}
	select {
	case <-provider.started:
	case <-time.After(time.Second):
		t.Fatal("auto-next prefetch did not start")
	}

	closed := make(chan struct{})
	go func() {
		service.Close()
		close(closed)
	}()
	select {
	case <-provider.canceled:
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel auto-next prefetch")
	}
	select {
	case <-closed:
		t.Fatal("Close returned before auto-next prefetch exited")
	default:
	}
	close(provider.finish)
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close did not return after auto-next prefetch exited")
	}
}

func startTestPlayback(t *testing.T, service *Service) {
	t.Helper()
	if _, err := service.Search(t.Context(), "frieren"); err != nil {
		t.Fatal(err)
	}
	episodes, err := service.Episodes(t.Context(), 154587)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.PlayEpisode(t.Context(), episodes[0]); err != nil {
		t.Fatal(err)
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for condition")
		}
		time.Sleep(time.Millisecond)
	}
}
