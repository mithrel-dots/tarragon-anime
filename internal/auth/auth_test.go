package auth

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAuthorizeURL(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "token"))
	raw, err := store.AuthorizationURL(DefaultClientID)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	if query.Get("client_id") != DefaultClientID || query.Get("response_type") != "token" || len(query.Get("state")) < 32 {
		t.Fatalf("authorize query = %#v", query)
	}
	// AniList rejects the request when a redirect is supplied explicitly.
	if _, ok := query["redirect_uri"]; ok {
		t.Fatalf("authorize query included redirect_uri: %#v", query)
	}
	info, err := os.Stat(store.authorizationStatePath())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("OAuth state permissions = %v", info.Mode().Perm())
	}
	secondRaw, err := store.AuthorizationURL(DefaultClientID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := url.Parse(secondRaw)
	if err != nil {
		t.Fatal(err)
	}
	if second.Query().Get("state") == query.Get("state") {
		t.Fatal("AuthorizationURL() reused OAuth state")
	}
	if _, err := store.AuthorizationURL(" "); err == nil {
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
			uri:  "tarragon-anime://auth#access_token=abc123&state=%s&token_type=Bearer&expires_in=31536000",
			want: "abc123",
		},
		{
			name: "query token",
			uri:  "tarragon-anime://auth?access_token=xyz789&state=%s",
			want: "xyz789",
		},
		{
			name: "denied",
			uri:  "tarragon-anime://auth?error=access_denied&error_description=User+denied&state=%s",
			err:  true,
		},
		{name: "missing token", uri: "tarragon-anime://auth?state=%s", err: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "token")
			starter := NewStore(path)
			authorizeURL, err := starter.AuthorizationURL(DefaultClientID)
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := url.Parse(authorizeURL)
			if err != nil {
				t.Fatal(err)
			}
			got, err := NewStore(path).ParseCallback(fmt.Sprintf(test.uri, url.QueryEscape(parsed.Query().Get("state"))))
			if (err != nil) != test.err {
				t.Fatalf("ParseCallback() error = %v, want error %v", err, test.err)
			}
			if got != test.want {
				t.Fatalf("ParseCallback() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestParseCallbackRequiresExactRedirectAndMatchingState(t *testing.T) {
	tests := []string{
		"https://auth#access_token=abc&state=%s",
		"tarragon-anime://other#access_token=abc&state=%s",
		"tarragon-anime://auth/#access_token=abc&state=%s",
		"tarragon-anime://auth:123#access_token=abc&state=%s",
		"tarragon-anime://user@auth#access_token=abc&state=%s",
	}
	for _, callback := range tests {
		t.Run(callback, func(t *testing.T) {
			store := NewStore(filepath.Join(t.TempDir(), "token"))
			raw, err := store.AuthorizationURL(DefaultClientID)
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := url.Parse(raw)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.ParseCallback(fmt.Sprintf(callback, parsed.Query().Get("state"))); err == nil {
				t.Fatal("ParseCallback() accepted an unexpected redirect")
			}
		})
	}

	path := filepath.Join(t.TempDir(), "token")
	store := NewStore(path)
	raw, err := store.AuthorizationURL(DefaultClientID)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(path).ParseCallback("tarragon-anime://auth#access_token=attacker&state=wrong"); err == nil || !strings.Contains(err.Error(), "did not match") {
		t.Fatalf("ParseCallback() mismatched state error = %v", err)
	}
	callback := "tarragon-anime://auth#access_token=legitimate&state=" + url.QueryEscape(parsed.Query().Get("state"))
	if token, err := NewStore(path).ParseCallback(callback); err != nil || token != "legitimate" {
		t.Fatalf("ParseCallback() after mismatch = %q, %v", token, err)
	}
}

func TestOAuthStateWorksAcrossProcessesAndIsOneTime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	raw, err := NewStore(path).AuthorizationURL(DefaultClientID)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	callback := "tarragon-anime://auth#access_token=abc&state=" + url.QueryEscape(parsed.Query().Get("state"))
	if token, err := NewStore(path).ParseCallback(callback); err != nil || token != "abc" {
		t.Fatalf("ParseCallback() = %q, %v", token, err)
	}
	if _, err := NewStore(path).ParseCallback(callback); err == nil {
		t.Fatal("ParseCallback() accepted a replayed callback")
	}
}

func TestOAuthStateMatchingCallbackIsAtomicallyOneTime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	raw, err := NewStore(path).AuthorizationURL(DefaultClientID)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	callback := "tarragon-anime://auth#access_token=abc&state=" + url.QueryEscape(parsed.Query().Get("state"))
	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			_, err := NewStore(path).ParseCallback(callback)
			results <- err
		}()
	}
	close(start)
	successes := 0
	for range 2 {
		if err := <-results; err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("matching callback successes = %d, want 1", successes)
	}
}

func TestParseCallbackRejectsExpiredState(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "token"))
	if err := os.WriteFile(store.authorizationStatePath(), []byte(fmt.Sprintf("%d\nexpired\n", time.Now().Add(-time.Minute).Unix())), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ParseCallback("tarragon-anime://auth#access_token=abc&state=expired"); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("ParseCallback() expired state error = %v", err)
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
