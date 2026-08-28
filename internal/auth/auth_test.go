package auth

import (
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

func TestAuthorizeURL(t *testing.T) {
	raw, err := AuthorizeURL(DefaultClientID)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	if query.Get("client_id") != DefaultClientID || query.Get("response_type") != "token" {
		t.Fatalf("authorize query = %#v", query)
	}
	// AniList rejects the request when a redirect is supplied explicitly.
	if _, ok := query["redirect_uri"]; ok {
		t.Fatalf("authorize query included redirect_uri: %#v", query)
	}
	if _, err := AuthorizeURL(" "); err == nil {
		t.Fatal("AuthorizeURL() accepted an empty client ID")
	}
}

func TestParseCallback(t *testing.T) {
	tests := []struct {
		name string
		uri  string
		want string
		err  bool
	}{
		{
			name: "fragment token",
			uri:  "tarragon-anime://auth#access_token=abc123&token_type=Bearer&expires_in=31536000",
			want: "abc123",
		},
		{
			name: "query token",
			uri:  "tarragon-anime://auth?access_token=xyz789",
			want: "xyz789",
		},
		{
			name: "denied",
			uri:  "tarragon-anime://auth?error=access_denied&error_description=User+denied",
			err:  true,
		},
		{name: "wrong scheme", uri: "https://anilist.co/#access_token=abc", err: true},
		{name: "missing token", uri: "tarragon-anime://auth", err: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := ParseCallback(test.uri)
			if (err != nil) != test.err {
				t.Fatalf("ParseCallback() error = %v, want error %v", err, test.err)
			}
			if got != test.want {
				t.Fatalf("ParseCallback() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestStoreSaveReloadAndDelete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "anime", "token")
	store := NewStore(path)

	token, err := store.Token()
	if err != nil || token != "" {
		t.Fatalf("Token() = %q, %v; want empty", token, err)
	}
	if err := store.Save("first-token"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("token permissions = %v", info.Mode().Perm())
	}
	if token, err = store.Token(); err != nil || token != "first-token" {
		t.Fatalf("Token() = %q, %v", token, err)
	}

	// A token replaced on disk must be picked up without a restart.
	if err := store.Save("second-token"); err != nil {
		t.Fatal(err)
	}
	if token, err = store.Token(); err != nil || token != "second-token" {
		t.Fatalf("Token() after refresh = %q, %v", token, err)
	}

	if err := store.Delete(); err != nil {
		t.Fatal(err)
	}
	if token, err = store.Token(); err != nil || token != "" {
		t.Fatalf("Token() after delete = %q, %v", token, err)
	}
	if err := store.Delete(); err != nil {
		t.Fatalf("Delete() on missing token = %v", err)
	}
	if err := store.Save("  "); err == nil {
		t.Fatal("Save() accepted an empty token")
	}
}

func TestStoreRejectsWorldReadableToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(path).Token(); err == nil {
		t.Fatal("Token() accepted world-readable permissions")
	}
}
