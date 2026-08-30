// Package auth implements AniList OAuth sign-in for the plugin.
//
// AniList only issues implicit-grant tokens to a registered redirect, so the
// plugin registers a custom URI scheme and receives the token in the URL
// fragment that the browser hands to the scheme handler.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
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
	authorizationTTL  = 10 * time.Minute
)

// AuthorizationURL persists a one-time state value and builds the AniList
// implicit-grant sign-in URL. The callback may be handled by another process.
func (s *Store) AuthorizationURL(clientID string) (string, error) {
	clientID = strings.TrimSpace(clientID)
	if clientID == "" {
		return "", errors.New("no AniList client ID is configured")
	}
	state, err := randomState()
	if err != nil {
		return "", fmt.Errorf("generate OAuth state: %w", err)
	}
	if err := s.saveAuthorizationState(state, time.Now().Add(authorizationTTL)); err != nil {
		return "", err
	}
	// AniList takes the redirect from the application settings; sending
	// redirect_uri here is rejected with unsupported_grant_type.
	query := url.Values{
		"client_id":     {clientID},
		"response_type": {"token"},
		"state":         {state},
	}
	return authorizeEndpoint + "?" + query.Encode(), nil
}

// ParseCallback validates and consumes the pending state before extracting the
// access token. AniList returns tokens in the fragment, while some URI handlers
// deliver the fragment as query parameters.
func (s *Store) ParseCallback(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("parse callback URI: %w", err)
	}
	if parsed.Scheme != Scheme {
		return "", fmt.Errorf("unexpected callback scheme %q", parsed.Scheme)
	}
	if parsed.Host != "auth" || parsed.Path != "" || parsed.User != nil {
		return "", fmt.Errorf("unexpected callback authority or path %q", parsed.Host+parsed.EscapedPath())
	}
	fragment, err := url.ParseQuery(parsed.Fragment)
	if err != nil {
		return "", fmt.Errorf("parse callback fragment: %w", err)
	}
	returnedState, err := callbackValue(parsed.Query(), fragment, "state")
	if err != nil {
		return "", err
	}
	if returnedState == "" {
		return "", errors.New("callback did not contain OAuth state")
	}
	expectedState, err := s.consumeAuthorizationState()
	if err != nil {
		return "", err
	}
	if subtle.ConstantTimeCompare([]byte(returnedState), []byte(expectedState)) != 1 {
		return "", errors.New("callback OAuth state did not match the pending login")
	}

	if message, err := callbackValue(parsed.Query(), fragment, "error"); err != nil {
		return "", err
	} else if message != "" {
		description, err := callbackValue(parsed.Query(), fragment, "error_description")
		if err != nil {
			return "", err
		}
		if description != "" {
			return "", fmt.Errorf("AniList denied authorization: %s: %s", message, description)
		}
		return "", fmt.Errorf("AniList denied authorization: %s", message)
	}
	token, err := callbackValue(parsed.Query(), fragment, "access_token")
	if err != nil {
		return "", err
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

func randomState() (string, error) {
	data := make([]byte, 32)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func callbackValue(query, fragment url.Values, name string) (string, error) {
	queryValue, fragmentValue := query.Get(name), fragment.Get(name)
	if queryValue != "" && fragmentValue != "" && queryValue != fragmentValue {
		return "", fmt.Errorf("callback contained conflicting %s values", name)
	}
	if fragmentValue != "" {
		return fragmentValue, nil
	}
	return queryValue, nil
}

func (s *Store) authorizationStatePath() string {
	return s.path + ".oauth-state"
}

func (s *Store) saveAuthorizationState(state string, expires time.Time) error {
	path := s.authorizationStatePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create OAuth state directory: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".oauth-state-*")
	if err != nil {
		return fmt.Errorf("create OAuth state temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("secure OAuth state file: %w", err)
	}
	if _, err := fmt.Fprintf(temporary, "%d\n%s\n", expires.Unix(), state); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write OAuth state: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close OAuth state file: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("store OAuth state: %w", err)
	}
	return nil
}

func (s *Store) consumeAuthorizationState() (string, error) {
	statePath := s.authorizationStatePath()
	suffix, err := randomState()
	if err != nil {
		return "", fmt.Errorf("generate OAuth state claim: %w", err)
	}
	claimedPath := statePath + ".consume-" + suffix
	if err := os.Rename(statePath, claimedPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", errors.New("no pending AniList login was found")
		}
		return "", fmt.Errorf("claim OAuth state: %w", err)
	}
	defer os.Remove(claimedPath)
	info, err := os.Stat(claimedPath)
	if err != nil {
		return "", fmt.Errorf("stat OAuth state: %w", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("OAuth state file %s must use permissions 0600", statePath)
	}
	data, err := os.ReadFile(claimedPath)
	if err != nil {
		return "", fmt.Errorf("read OAuth state: %w", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 || lines[1] == "" {
		return "", errors.New("pending OAuth state is invalid")
	}
	expires, err := strconv.ParseInt(lines[0], 10, 64)
	if err != nil {
		return "", errors.New("pending OAuth state is invalid")
	}
	if time.Now().Unix() > expires {
		return "", errors.New("pending AniList login has expired")
	}
	return lines[1], nil
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
