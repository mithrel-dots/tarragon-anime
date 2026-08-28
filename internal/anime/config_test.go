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

[mpv]
args = ["--profile=anime,fast", "--script-opts=value#part"] # comment
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Provider != "allanime" || got.PreferredQuality != "worst" || got.Translation != "dub" {
		t.Fatalf("LoadConfig() = %#v", got)
	}
	wantArgs := []string{"--profile=anime,fast", "--script-opts=value#part"}
	if !reflect.DeepEqual(got.MPVArgs, wantArgs) {
		t.Fatalf("MPVArgs = %#v, want %#v", got.MPVArgs, wantArgs)
	}
}

func TestLoadConfigMissingUsesDefaults(t *testing.T) {
	got, err := LoadConfig(filepath.Join(t.TempDir(), "missing.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Provider != "allanime" || got.PreferredQuality != "best" || got.Translation != "sub" || len(got.MPVArgs) != 0 {
		t.Fatalf("LoadConfig() = %#v", got)
	}
}
