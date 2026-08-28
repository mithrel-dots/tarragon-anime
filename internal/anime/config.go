package anime

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Config struct {
	Provider         string
	PreferredQuality string
	Translation      string
	AutoNext         bool
	Resume           bool
	ResumeRewind     float64
	MPVArgs          []string
	Sync             SyncConfig
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
		PreferredQuality: "best",
		Translation:      "sub",
		AutoNext:         true,
		Resume:           true,
		ResumeRewind:     5,
		Sync: SyncConfig{
			Enabled: true, Conflict: "highest", Trigger: "eof", ThresholdPercent: 90,
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
			cfg.Provider, err = parseString(value)
		case ".preferred_quality":
			cfg.PreferredQuality, err = parseString(value)
		case ".translation":
			cfg.Translation, err = parseString(value)
		case ".auto_next":
			cfg.AutoNext, err = strconv.ParseBool(value)
		case ".resume":
			cfg.Resume, err = strconv.ParseBool(value)
		case ".resume_rewind_seconds":
			cfg.ResumeRewind, err = strconv.ParseFloat(value, 64)
		case "mpv.args":
			cfg.MPVArgs, err = parseStringArray(value)
		case "sync.enabled":
			cfg.Sync.Enabled, err = strconv.ParseBool(value)
		case "sync.conflict":
			cfg.Sync.Conflict, err = parseString(value)
		case "sync.trigger":
			cfg.Sync.Trigger, err = parseString(value)
		case "sync.threshold_percent":
			cfg.Sync.ThresholdPercent, err = strconv.ParseFloat(value, 64)
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
	if cfg.Provider != "allanime" {
		return Config{}, fmt.Errorf("unsupported provider %q", cfg.Provider)
	}
	if cfg.Translation != "sub" && cfg.Translation != "dub" {
		return Config{}, fmt.Errorf("translation must be %q or %q", "sub", "dub")
	}
	if cfg.ResumeRewind < 0 {
		return Config{}, fmt.Errorf("resume_rewind_seconds must not be negative")
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
	return cfg, nil
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

func LoadToken(path string) (string, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("stat AniList token: %w", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("AniList token file %s must use permissions 0600", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read AniList token: %w", err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", fmt.Errorf("AniList token file %s is empty", path)
	}
	return token, nil
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
		return nil, nil
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
