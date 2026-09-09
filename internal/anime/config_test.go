package anime

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
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

func TestLoadConfigBrowserSection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "anime.toml")
	content := `[browser]
enabled = false
binary = "/usr/bin/chromium"
profile_dir = "/tmp/anime-browser"
headless = false
timeout_seconds = 30
idle_timeout_seconds = 90
max_sessions = 3
auto_clearance = false
clearance_wait_seconds = 10
clearance_timeout_seconds = 120
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	want := BrowserConfig{
		Enabled: false, Binary: "/usr/bin/chromium", ProfileDir: "/tmp/anime-browser",
		Headless: false, Timeout: 30 * time.Second, IdleTimeout: 90 * time.Second, MaxSessions: 3,
		AutoClearance: false, ClearanceWait: 10 * time.Second, ClearanceTimeout: 2 * time.Minute,
	}
	if !reflect.DeepEqual(got.Browser, want) {
		t.Fatalf("Browser = %#v, want %#v", got.Browser, want)
	}
}

func TestLoadConfigBrowserDefaults(t *testing.T) {
	want := BrowserConfig{
		Enabled: true, Headless: true,
		Timeout: 45 * time.Second, IdleTimeout: 2 * time.Minute, MaxSessions: 2,
		AutoClearance: true, ClearanceWait: 20 * time.Second, ClearanceTimeout: 5 * time.Minute,
	}
	for name, content := range map[string]string{
		"missing": "",
		"empty":   "",
		"legacy":  "[browser]\nenabled = true\nheadless = true\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "anime.toml")
			if name != "missing" {
				if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			got, err := LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.Browser, want) {
				t.Fatalf("Browser = %#v, want %#v", got.Browser, want)
			}
		})
	}
}

func TestLoadConfigBrowserClearanceEqualLimits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "anime.toml")
	content := "[browser]\nauto_clearance = true\nclearance_wait_seconds = 20.5\nclearance_timeout_seconds = 20.5\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	want := 20500 * time.Millisecond
	if !got.Browser.AutoClearance || got.Browser.ClearanceWait != want || got.Browser.ClearanceTimeout != want {
		t.Fatalf("Browser = %#v, want auto clearance with equal %s limits", got.Browser, want)
	}
}

func TestLoadConfigRejectsInvalidBrowserLimits(t *testing.T) {
	for name, content := range map[string]string{
		"timeout":                      "[browser]\ntimeout_seconds = 0\n",
		"idle timeout":                 "[browser]\nidle_timeout_seconds = -1\n",
		"max sessions":                 "[browser]\nmax_sessions = 0\n",
		"auto clearance":               "[browser]\nauto_clearance = invalid\n",
		"clearance wait":               "[browser]\nclearance_wait_seconds = 0\n",
		"negative clearance wait":      "[browser]\nclearance_wait_seconds = -1\n",
		"malformed clearance wait":     "[browser]\nclearance_wait_seconds = invalid\n",
		"clearance timeout":            "[browser]\nclearance_timeout_seconds = 5\n",
		"zero clearance timeout":       "[browser]\nclearance_timeout_seconds = 0\n",
		"negative clearance timeout":   "[browser]\nclearance_timeout_seconds = -1\n",
		"malformed clearance timeout":  "[browser]\nclearance_timeout_seconds = invalid\n",
		"wait exceeds default timeout": "[browser]\nclearance_wait_seconds = 301\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "anime.toml")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadConfig(path); err == nil {
				t.Fatalf("LoadConfig() error = nil, want a rejection of %s", name)
			}
		})
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
