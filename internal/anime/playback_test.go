package anime

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

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

type loadCall struct {
	stream mpv.Stream
	title  string
	start  float64
}

type fakeSession struct {
	events chan mpv.Event

	mu        sync.Mutex
	loads     []loadCall
	subtitles []string
	messages  []string
	closed    bool
}

func newFakeSession() *fakeSession {
	return &fakeSession{events: make(chan mpv.Event, 16)}
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

func (s *fakeSession) ShowText(_ context.Context, message string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages = append(s.messages, message)
	return nil
}

func (s *fakeSession) Close() {
	s.mu.Lock()
	s.closed = true
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

type fakePlayer struct {
	session *fakeSession
	start   float64
}

func (p *fakePlayer) Play(_ context.Context, _ mpv.Stream, _ string, start float64) (mpv.SessionController, error) {
	p.start = start
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
	time.Sleep(20 * time.Millisecond)
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
