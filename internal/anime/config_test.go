package anime

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "anime.toml")
	content := `provider = "allanime"
preferred_quality = "worst"
translation = "dub"
auto_next = false
auto_next_threshold_percent = 82.5
resume = true
resume_rewind_seconds = 12.5

[mpv]
args = ["--profile=anime,fast", "--script-opts=value#part"] # comment

[skip]
enabled = true
intro = false
outro = true
margin_seconds = 2.5

[sync]
enabled = true
conflict = "local"
trigger = "threshold"
threshold_percent = 87.5
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Provider != "allanime" || got.PreferredQuality != "worst" || got.Translation != "dub" || got.AutoNext || got.AutoNextThresholdPercent != 82.5 || !got.Resume || got.ResumeRewind != 12.5 {
		t.Fatalf("LoadConfig() = %#v", got)
	}
	if !reflect.DeepEqual(got.Providers, []string{"allanime"}) {
		t.Fatalf("Providers = %#v", got.Providers)
	}
	wantArgs := []string{"--profile=anime,fast", "--script-opts=value#part"}
	if !reflect.DeepEqual(got.MPVArgs, wantArgs) {
		t.Fatalf("MPVArgs = %#v, want %#v", got.MPVArgs, wantArgs)
	}
	if !got.Skip.Enabled || got.Skip.Intro || !got.Skip.Outro || got.Skip.MarginSeconds != 2.5 {
		t.Fatalf("Skip = %#v", got.Skip)
	}
	if !got.Sync.Enabled || got.Sync.Conflict != "local" || got.Sync.Trigger != "threshold" || got.Sync.ThresholdPercent != 87.5 {
		t.Fatalf("Sync = %#v", got.Sync)
	}
}

func TestLoadConfigMissingUsesDefaults(t *testing.T) {
	got, err := LoadConfig(filepath.Join(t.TempDir(), "missing.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Provider != "allanime" || got.PreferredQuality != "best" || got.Translation != "sub" || !got.AutoNext || got.AutoNextThresholdPercent != 80 || !got.Resume || got.ResumeRewind != 5 || len(got.MPVArgs) != 0 {
		t.Fatalf("LoadConfig() = %#v", got)
	}
	if !reflect.DeepEqual(got.Providers, []string{"allanime"}) {
		t.Fatalf("Providers = %#v", got.Providers)
	}
	if !got.Sync.Enabled || got.Sync.Conflict != "highest" || got.Sync.Trigger != "eof" || got.Sync.ThresholdPercent != 90 {
		t.Fatalf("Sync = %#v", got.Sync)
	}
	if !got.Skip.Enabled || !got.Skip.Intro || !got.Skip.Outro || got.Skip.MarginSeconds != 0 {
		t.Fatalf("Skip = %#v", got.Skip)
	}
}

func TestLoadConfigProviderOrderCanBeEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "anime.toml")
	if err := os.WriteFile(path, []byte("providers = []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Provider != "" || got.Providers == nil || len(got.Providers) != 0 {
		t.Fatalf("LoadConfig() = %#v", got)
	}
}

func TestLoadConfigProviderOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "anime.toml")
	if err := os.WriteFile(path, []byte("providers = [\"animepahe\", \"allanime\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Providers, []string{"animepahe", "allanime"}) || got.Provider != "animepahe" {
		t.Fatalf("LoadConfig() = %#v", got)
	}
}

func TestLoadConfigRejectsUnknownProvider(t *testing.T) {
	path := filepath.Join(t.TempDir(), "anime.toml")
	if err := os.WriteFile(path, []byte("providers = [\"unknown\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("LoadConfig() accepted an unknown provider")
	}
}
