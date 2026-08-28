// Package auth implements AniList OAuth sign-in for the plugin.
//
// AniList only issues implicit-grant tokens to a registered redirect, so the
// plugin registers a custom URI scheme and receives the token in the URL
// fragment that the browser hands to the scheme handler.
package auth

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// Scheme must match the redirect URI registered with AniList.
	Scheme      = "tarragon-anime"
	RedirectURI = Scheme + "://auth"

	// DefaultClientID is the public AniList application registered for this
	// plugin. Client secrets are never needed for the implicit grant.
	DefaultClientID = "49699"

	authorizeEndpoint = "https://anilist.co/api/v2/oauth/authorize"
)

// AuthorizeURL builds the AniList implicit-grant sign-in URL.
func AuthorizeURL(clientID string) (string, error) {
	clientID = strings.TrimSpace(clientID)
	if clientID == "" {
		return "", errors.New("no AniList client ID is configured")
	}
	// AniList's implicit grant accepts only these parameters and takes the
	// redirect from the application settings; sending redirect_uri here is
	// rejected with unsupported_grant_type.
	query := url.Values{
		"client_id":     {clientID},
		"response_type": {"token"},
	}
	return authorizeEndpoint + "?" + query.Encode(), nil
}

// ParseCallback extracts the access token from a redirect URI. AniList returns
// implicit-grant tokens in the fragment, but errors arrive as query parameters.
func ParseCallback(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("parse callback URI: %w", err)
	}
	if parsed.Scheme != Scheme {
		return "", fmt.Errorf("unexpected callback scheme %q", parsed.Scheme)
	}
	if message := parsed.Query().Get("error"); message != "" {
		if description := parsed.Query().Get("error_description"); description != "" {
			return "", fmt.Errorf("AniList denied authorization: %s: %s", message, description)
		}
		return "", fmt.Errorf("AniList denied authorization: %s", message)
	}
	values, err := url.ParseQuery(parsed.Fragment)
	if err != nil {
		return "", fmt.Errorf("parse callback fragment: %w", err)
	}
	token := values.Get("access_token")
	if token == "" {
		// Some handlers deliver the fragment as a query string instead.
		token = parsed.Query().Get("access_token")
	}
	if token == "" {
		return "", errors.New("callback did not contain an access token")
	}
	return token, nil
}

// Store persists the access token with owner-only permissions and reloads it
// when the file changes, so signing in does not require a plugin restart.
type Store struct {
	path string

	mu      sync.Mutex
	token   string
	modTime time.Time
	size    int64
	loaded  bool
}

func NewStore(path string) *Store {
	return &Store{path: path}
}

func (s *Store) Path() string {
	return s.path
}

// Token returns the stored token, or an empty string when signed out.
func (s *Store) Token() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	info, err := os.Stat(s.path)
	if errors.Is(err, os.ErrNotExist) {
		s.token, s.loaded = "", true
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("stat AniList token: %w", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("AniList token file %s must use permissions 0600", s.path)
	}
	if s.loaded && info.ModTime().Equal(s.modTime) && info.Size() == s.size {
		return s.token, nil
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		return "", fmt.Errorf("read AniList token: %w", err)
	}
	s.token = strings.TrimSpace(string(data))
	s.modTime, s.size, s.loaded = info.ModTime(), info.Size(), true
	return s.token, nil
}

func (s *Store) Save(token string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return errors.New("refusing to store an empty AniList token")
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create token directory: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(s.path), ".token-*")
	if err != nil {
		return fmt.Errorf("create token temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("secure token file: %w", err)
	}
	if _, err := temporary.WriteString(token + "\n"); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write token: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close token file: %w", err)
	}
	if err := os.Rename(temporaryPath, s.path); err != nil {
		return fmt.Errorf("store token: %w", err)
	}
	s.mu.Lock()
	s.loaded = false
	s.mu.Unlock()
	return nil
}

func (s *Store) Delete() error {
	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove AniList token: %w", err)
	}
	s.mu.Lock()
	s.token, s.loaded = "", false
	s.mu.Unlock()
	return nil
}
