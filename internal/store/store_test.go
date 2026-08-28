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
