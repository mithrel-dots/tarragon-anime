package store

import (
	"path/filepath"
	"testing"
)

func TestStorePersistsMappingsAndProgress(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "anime.db")
	state, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.SaveProviderMapping(t.Context(), 154587, "allanime", "show-1"); err != nil {
		t.Fatal(err)
	}
	wantProgress := Progress{MediaID: 154587, Episode: 4, Position: 317.5, Duration: 1440, Complete: false}
	if err := state.SaveProgress(t.Context(), wantProgress); err != nil {
		t.Fatal(err)
	}
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}

	state, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	providerID, found, err := state.ProviderMapping(t.Context(), 154587, "allanime")
	if err != nil {
		t.Fatal(err)
	}
	if !found || providerID != "show-1" {
		t.Fatalf("ProviderMapping() = %q, %v", providerID, found)
	}
	progress, found, err := state.Progress(t.Context(), 154587, 4)
	if err != nil {
		t.Fatal(err)
	}
	if !found || progress != wantProgress {
		t.Fatalf("Progress() = %#v, %v, want %#v", progress, found, wantProgress)
	}
}

func TestStoreResumeEntriesAndSyncQueue(t *testing.T) {
	state, err := Open(filepath.Join(t.TempDir(), "anime.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	if err := state.SaveMedia(t.Context(), MediaInfo{ID: 1, Title: "Frieren", PreviewPath: "/tmp/1.jpg", Episodes: 28}); err != nil {
		t.Fatal(err)
	}
	if err := state.SaveProgress(t.Context(), Progress{MediaID: 1, Episode: 3, Position: 120, Duration: 1400}); err != nil {
		t.Fatal(err)
	}
	if err := state.SaveProgress(t.Context(), Progress{MediaID: 1, Episode: 4, Position: 30, Duration: 1400}); err != nil {
		t.Fatal(err)
	}
	entries, err := state.ResumeEntries(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Episode != 4 || entries[0].Title != "Frieren" || entries[0].TotalEpisodes != 28 {
		t.Fatalf("ResumeEntries() = %#v", entries)
	}
	progress, found, err := state.MediaProgress(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if !found || progress.Episode != 4 {
		t.Fatalf("MediaProgress() = %#v, %v", progress, found)
	}

	if err := state.EnqueueSync(t.Context(), 1, 4); err != nil {
		t.Fatal(err)
	}
	if err := state.EnqueueSync(t.Context(), 1, 4); err != nil {
		t.Fatal(err)
	}
	items, err := state.PendingSyncs(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Episode != 4 {
		t.Fatalf("PendingSyncs() = %#v", items)
	}
	if err := state.RecordSyncFailure(t.Context(), items[0].ID, "offline"); err != nil {
		t.Fatal(err)
	}
	items, err = state.PendingSyncs(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Attempts != 1 {
		t.Fatalf("PendingSyncs() after failure = %#v", items)
	}
	if err := state.DeleteSync(t.Context(), items[0].ID); err != nil {
		t.Fatal(err)
	}
	items, err = state.PendingSyncs(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("PendingSyncs() after delete = %#v", items)
	}
}

func TestStoreUpdatesProgress(t *testing.T) {
	state, err := Open(filepath.Join(t.TempDir(), "anime.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	progress := Progress{MediaID: 1, Episode: 2, Position: 10, Duration: 20}
	if err := state.SaveProgress(t.Context(), progress); err != nil {
		t.Fatal(err)
	}
	progress.Position = 20
	progress.Complete = true
	if err := state.SaveProgress(t.Context(), progress); err != nil {
		t.Fatal(err)
	}
	got, found, err := state.Progress(t.Context(), 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !found || got != progress {
		t.Fatalf("Progress() = %#v, %v, want %#v", got, found, progress)
	}
}
