package anime

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"tarragon-anime/internal/auth"
)

type Config struct {
	Provider                 string
	Providers                []string
	PreferredQuality         string
	Translation              string
	AutoNext                 bool
	AutoNextThresholdPercent float64
	Resume                   bool
	ResumeRewind             float64
	ClientID                 string
	MPVArgs                  []string
	Skip                     SkipConfig
	Sync                     SyncConfig
	Browser                  BrowserConfig
}

// BrowserConfig controls the managed Chromium instance used to resolve streams
// from a provider's own player.
type BrowserConfig struct {
	Enabled     bool
	Binary      string
	ProfileDir  string
	Headless    bool
	Timeout     time.Duration
	IdleTimeout time.Duration
	MaxSessions int
	// AutoClearance opens a visible window when the origin serves a bot
	// check, instead of only reporting how to pass it by hand.
	AutoClearance bool
	// ClearanceWait is how long resolution waits for the check to clear
	// before reporting that the window needs attention.
	ClearanceWait time.Duration
	// ClearanceTimeout is how long that window stays open.
	ClearanceTimeout time.Duration
}

type SkipConfig struct {
	Enabled       bool
	Intro         bool
	Outro         bool
	MarginSeconds float64
}

type SyncConfig struct {
	Enabled          bool
	Conflict         string
	Trigger          string
	ThresholdPercent float64
}

func DefaultConfig() Config {
	return Config{
		Provider:         "allanime",
		Providers:        []string{"allanime"},
		PreferredQuality: "best",
		Translation:      "sub",
		AutoNext:         true,
		Resume:           true,
		ResumeRewind:     5,
		ClientID:         auth.DefaultClientID,
		Sync: SyncConfig{
			Enabled: true, Conflict: "highest", Trigger: "eof", ThresholdPercent: 90,
		},
		AutoNextThresholdPercent: 80,
		Skip:                     SkipConfig{Enabled: true, Intro: true, Outro: true},
		Browser: BrowserConfig{
			Enabled: true, Headless: true,
			Timeout: 45 * time.Second, IdleTimeout: 2 * time.Minute, MaxSessions: 2,
			AutoClearance: true, ClearanceWait: 20 * time.Second, ClearanceTimeout: 5 * time.Minute,
		},
	}
}

func ConfigPath() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "tarragon", "anime.toml"), nil
}

func LoadConfig(path string) (Config, error) {
	cfg := DefaultConfig()
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("open config: %w", err)
	}
	defer f.Close()

	section := ""
	scanner := bufio.NewScanner(f)
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(stripComment(scanner.Text()))
		if text == "" {
			continue
		}
		if strings.HasPrefix(text, "[") && strings.HasSuffix(text, "]") {
			section = strings.TrimSpace(text[1 : len(text)-1])
			continue
		}
		key, value, ok := strings.Cut(text, "=")
		if !ok {
			return Config{}, fmt.Errorf("parse config line %d: expected key = value", line)
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		switch section + "." + key {
		case ".provider":
			if strings.HasPrefix(strings.TrimSpace(value), "[") {
				cfg.Providers, err = parseStringArray(value)
				if len(cfg.Providers) > 0 {
					cfg.Provider = cfg.Providers[0]
				} else {
					cfg.Provider = ""
				}
			} else {
				cfg.Provider, err = parseString(value)
				if err == nil {
					cfg.Providers = []string{cfg.Provider}
				}
			}
		case ".providers":
			cfg.Providers, err = parseStringArray(value)
			if len(cfg.Providers) > 0 {
				cfg.Provider = cfg.Providers[0]
			} else {
				cfg.Provider = ""
			}
		case ".preferred_quality":
			cfg.PreferredQuality, err = parseString(value)
		case ".translation":
			cfg.Translation, err = parseString(value)
		case ".auto_next":
			cfg.AutoNext, err = strconv.ParseBool(value)
		case ".auto_next_threshold_percent":
			cfg.AutoNextThresholdPercent, err = strconv.ParseFloat(value, 64)
		case ".resume":
			cfg.Resume, err = strconv.ParseBool(value)
		case ".resume_rewind_seconds":
			cfg.ResumeRewind, err = strconv.ParseFloat(value, 64)
		case "anilist.client_id":
			cfg.ClientID, err = parseString(value)
		case "mpv.args":
			cfg.MPVArgs, err = parseStringArray(value)
		case "skip.enabled":
			cfg.Skip.Enabled, err = strconv.ParseBool(value)
		case "skip.intro":
			cfg.Skip.Intro, err = strconv.ParseBool(value)
		case "skip.outro":
			cfg.Skip.Outro, err = strconv.ParseBool(value)
		case "skip.margin_seconds":
			cfg.Skip.MarginSeconds, err = strconv.ParseFloat(value, 64)
		case "sync.enabled":
			cfg.Sync.Enabled, err = strconv.ParseBool(value)
		case "sync.conflict":
			cfg.Sync.Conflict, err = parseString(value)
		case "sync.trigger":
			cfg.Sync.Trigger, err = parseString(value)
		case "sync.threshold_percent":
			cfg.Sync.ThresholdPercent, err = strconv.ParseFloat(value, 64)
		case "browser.enabled":
			cfg.Browser.Enabled, err = strconv.ParseBool(value)
		case "browser.binary":
			cfg.Browser.Binary, err = parseString(value)
		case "browser.profile_dir":
			cfg.Browser.ProfileDir, err = parseString(value)
		case "browser.headless":
			cfg.Browser.Headless, err = strconv.ParseBool(value)
		case "browser.timeout_seconds":
			cfg.Browser.Timeout, err = parseDuration(value)
		case "browser.idle_timeout_seconds":
			cfg.Browser.IdleTimeout, err = parseDuration(value)
		case "browser.max_sessions":
			cfg.Browser.MaxSessions, err = strconv.Atoi(value)
		case "browser.auto_clearance":
			cfg.Browser.AutoClearance, err = strconv.ParseBool(value)
		case "browser.clearance_wait_seconds":
			cfg.Browser.ClearanceWait, err = parseDuration(value)
		case "browser.clearance_timeout_seconds":
			cfg.Browser.ClearanceTimeout, err = parseDuration(value)
		default:
			continue
		}
		if err != nil {
			return Config{}, fmt.Errorf("parse config line %d: %w", line, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	for index, name := range cfg.Providers {
		name = strings.ToLower(strings.TrimSpace(name))
		cfg.Providers[index] = name
		if name != "allanime" && name != "animepahe" {
			return Config{}, fmt.Errorf("unsupported provider %q", name)
		}
	}
	if len(cfg.Providers) > 0 {
		cfg.Provider = cfg.Providers[0]
	}
	if cfg.Translation != "sub" && cfg.Translation != "dub" {
		return Config{}, fmt.Errorf("translation must be %q or %q", "sub", "dub")
	}
	if cfg.ResumeRewind < 0 {
		return Config{}, fmt.Errorf("resume_rewind_seconds must not be negative")
	}
	if cfg.AutoNextThresholdPercent <= 0 || cfg.AutoNextThresholdPercent > 100 {
		return Config{}, fmt.Errorf("auto_next_threshold_percent must be greater than 0 and at most 100")
	}
	if cfg.Skip.MarginSeconds < 0 {
		return Config{}, fmt.Errorf("skip.margin_seconds must not be negative")
	}
	if cfg.Sync.Conflict != "highest" && cfg.Sync.Conflict != "local" && cfg.Sync.Conflict != "remote" {
		return Config{}, fmt.Errorf("sync.conflict must be %q, %q, or %q", "highest", "local", "remote")
	}
	if cfg.Sync.Trigger != "eof" && cfg.Sync.Trigger != "threshold" && cfg.Sync.Trigger != "start" {
		return Config{}, fmt.Errorf("sync.trigger must be %q, %q, or %q", "eof", "threshold", "start")
	}
	if cfg.Sync.ThresholdPercent <= 0 || cfg.Sync.ThresholdPercent > 100 {
		return Config{}, fmt.Errorf("sync.threshold_percent must be greater than 0 and at most 100")
	}
	if cfg.Browser.Timeout <= 0 {
		return Config{}, fmt.Errorf("browser.timeout_seconds must be greater than 0")
	}
	if cfg.Browser.IdleTimeout <= 0 {
		return Config{}, fmt.Errorf("browser.idle_timeout_seconds must be greater than 0")
	}
	if cfg.Browser.MaxSessions <= 0 {
		return Config{}, fmt.Errorf("browser.max_sessions must be greater than 0")
	}
	if cfg.Browser.ClearanceWait <= 0 {
		return Config{}, fmt.Errorf("browser.clearance_wait_seconds must be greater than 0")
	}
	if cfg.Browser.ClearanceTimeout < cfg.Browser.ClearanceWait {
		return Config{}, fmt.Errorf("browser.clearance_timeout_seconds must be at least browser.clearance_wait_seconds")
	}
	return cfg, nil
}

func parseDuration(value string) (time.Duration, error) {
	seconds, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, err
	}
	return time.Duration(seconds * float64(time.Second)), nil
}

func TokenPath() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "tarragon", "anime", "token"), nil
}

func parseString(value string) (string, error) {
	parsed, err := strconv.Unquote(value)
	if err != nil {
		return "", fmt.Errorf("expected quoted string: %w", err)
	}
	return parsed, nil
}

func parseStringArray(value string) ([]string, error) {
	value = strings.TrimSpace(value)
	if len(value) < 2 || value[0] != '[' || value[len(value)-1] != ']' {
		return nil, fmt.Errorf("expected string array")
	}
	value = strings.TrimSpace(value[1 : len(value)-1])
	if value == "" {
		return []string{}, nil
	}
	parts, err := splitArray(value)
	if err != nil {
		return nil, err
	}
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		item, err := parseString(strings.TrimSpace(part))
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, nil
}

func stripComment(value string) string {
	inString, escaped := false, false
	for index, r := range value {
		if escaped {
			escaped = false
			continue
		}
		if r == '\\' && inString {
			escaped = true
			continue
		}
		if r == '"' {
			inString = !inString
			continue
		}
		if r == '#' && !inString {
			return value[:index]
		}
	}
	return value
}

func splitArray(value string) ([]string, error) {
	var parts []string
	start, inString, escaped := 0, false, false
	for index, r := range value {
		if escaped {
			escaped = false
			continue
		}
		if r == '\\' && inString {
			escaped = true
			continue
		}
		if r == '"' {
			inString = !inString
			continue
		}
		if r == ',' && !inString {
			parts = append(parts, strings.TrimSpace(value[start:index]))
			start = index + 1
		}
	}
	if inString {
		return nil, fmt.Errorf("unterminated string")
	}
	parts = append(parts, strings.TrimSpace(value[start:]))
	return parts, nil
}
