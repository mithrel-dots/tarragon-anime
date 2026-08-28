package anime

import (
	"context"
	"errors"
	"sync"
	"testing"

	"tarragon-anime/internal/anilist"
	"tarragon-anime/internal/store"
)

type fakeSync struct {
	mu     sync.Mutex
	remote int
	found  bool
	fail   error
	saved  []int
	status string
}

func (f *fakeSync) ListEntry(context.Context, int) (anilist.ListEntry, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return anilist.ListEntry{}, false, f.fail
	}
	return anilist.ListEntry{Progress: f.remote, Status: "CURRENT"}, f.found, nil
}

func (f *fakeSync) SaveProgress(_ context.Context, _ int, progress int, status string) (anilist.ListEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return anilist.ListEntry{}, f.fail
	}
	f.saved = append(f.saved, progress)
	f.status = status
	return anilist.ListEntry{Progress: progress, Status: status}, nil
}

func (f *fakeSync) List(context.Context, int, string) ([]anilist.ListItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return nil, f.fail
	}
	return []anilist.ListItem{{Media: anilist.Media{ID: 154587, Title: "Frieren", Episodes: 28}, Status: "CURRENT", Progress: 4}}, nil
}

func (f *fakeSync) SetStatus(context.Context, int, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fail
}

func (f *fakeSync) Viewer(context.Context) (anilist.Viewer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return anilist.Viewer{}, f.fail
	}
	return anilist.Viewer{ID: 1, Name: "mithrel"}, nil
}

func (f *fakeSync) savedProgress() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.saved...)
}

func newSyncService(t *testing.T, state *memoryState, syncer syncClient, config Config) *Service {
	t.Helper()
	return NewService(&cachingAniList{}, navigationProvider{}, &fakePlayer{session: newFakeSession()}, state, nil, syncer, config, nil)
}

func TestFlushSyncConflictPolicies(t *testing.T) {
	tests := []struct {
		name     string
		conflict string
		remote   int
		found    bool
		want     []int
	}{
		{name: "highest keeps remote lead", conflict: "highest", remote: 9, found: true, want: nil},
		{name: "highest pushes local lead", conflict: "highest", remote: 1, found: true, want: []int{4}},
		{name: "local overwrites remote", conflict: "local", remote: 9, found: true, want: []int{4}},
		{name: "remote wins", conflict: "remote", remote: 9, found: true, want: nil},
		{name: "no remote entry", conflict: "highest", found: false, want: []int{4}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := newMemoryState()
			if err := state.EnqueueSync(t.Context(), 154587, 4); err != nil {
				t.Fatal(err)
			}
			syncer := &fakeSync{remote: test.remote, found: test.found}
			config := DefaultConfig()
			config.Sync.Conflict = test.conflict
			newSyncService(t, state, syncer, config).FlushSync(t.Context())

			if got := syncer.savedProgress(); len(got) != len(test.want) {
				t.Fatalf("saved = %v, want %v", got, test.want)
			} else if len(got) == 1 && got[0] != test.want[0] {
				t.Fatalf("saved = %v, want %v", got, test.want)
			}
			if state.pendingCount() != 0 {
				t.Fatalf("queue not drained: %d", state.pendingCount())
			}
		})
	}
}

func TestFlushSyncKeepsQueueWhenOffline(t *testing.T) {
	state := newMemoryState()
	if err := state.EnqueueSync(t.Context(), 154587, 4); err != nil {
		t.Fatal(err)
	}
	syncer := &fakeSync{fail: errors.New("network unreachable")}
	newSyncService(t, state, syncer, DefaultConfig()).FlushSync(t.Context())
	if state.pendingCount() != 1 {
		t.Fatalf("queue length = %d, want 1", state.pendingCount())
	}
	if len(state.failures) != 1 {
		t.Fatalf("failures = %v", state.failures)
	}

	syncer.mu.Lock()
	syncer.fail = nil
	syncer.mu.Unlock()
	newSyncService(t, state, syncer, DefaultConfig()).FlushSync(t.Context())
	if state.pendingCount() != 0 || len(syncer.savedProgress()) != 1 {
		t.Fatalf("retry failed: queue=%d saved=%v", state.pendingCount(), syncer.savedProgress())
	}
}

func TestSyncMarksCompletedOnFinalEpisode(t *testing.T) {
	state := newMemoryState()
	if err := state.EnqueueSync(t.Context(), 154587, 28); err != nil {
		t.Fatal(err)
	}
	syncer := &fakeSync{}
	newSyncService(t, state, syncer, DefaultConfig()).FlushSync(t.Context())
	if syncer.status != "COMPLETED" {
		t.Fatalf("status = %q, want COMPLETED", syncer.status)
	}
}

func TestSyncDisabledWithoutToken(t *testing.T) {
	state := newMemoryState()
	if err := state.EnqueueSync(t.Context(), 1, 1); err != nil {
		t.Fatal(err)
	}
	newSyncService(t, state, nil, DefaultConfig()).FlushSync(t.Context())
	if state.pendingCount() != 1 {
		t.Fatal("queue drained without an AniList token")
	}
}

func TestContinueWatchingAdvancesAfterCompletedEpisode(t *testing.T) {
	state := newMemoryState()
	if err := state.SaveMedia(t.Context(), store.MediaInfo{ID: 154587, Title: "Frieren", Episodes: 28}); err != nil {
		t.Fatal(err)
	}
	if err := state.SaveProgress(t.Context(), store.Progress{MediaID: 154587, Episode: 4, Complete: true}); err != nil {
		t.Fatal(err)
	}
	entries, err := newSyncService(t, state, nil, DefaultConfig()).ContinueWatching(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Episode != 5 || entries[0].Continues {
		t.Fatalf("ContinueWatching() = %#v", entries)
	}
}
