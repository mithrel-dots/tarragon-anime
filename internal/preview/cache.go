package preview

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const maxImageSize = 10 << 20

type Cache struct {
	http *http.Client
	dir  string
}

func NewCache(httpClient *http.Client, dir string) *Cache {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Cache{http: httpClient, dir: dir}
}

func DefaultDir() (string, error) {
	base := os.Getenv("XDG_CACHE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		base = filepath.Join(home, ".cache")
	}
	return filepath.Join(base, "tarragon", "anime", "covers"), nil
}

func (c *Cache) Get(ctx context.Context, mediaID int, imageURL string) (string, error) {
	if strings.TrimSpace(imageURL) == "" {
		return "", nil
	}
	if mediaID <= 0 {
		return "", fmt.Errorf("invalid AniList media ID %d", mediaID)
	}
	if err := os.MkdirAll(c.dir, 0o700); err != nil {
		return "", fmt.Errorf("create preview cache: %w", err)
	}
	path := filepath.Join(c.dir, strconv.Itoa(mediaID)+".jpg")
	if info, err := os.Stat(path); err == nil && info.Size() > 0 {
		return path, nil
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("stat preview cache: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, imageURL, nil)
	if err != nil {
		return "", fmt.Errorf("create preview request: %w", err)
	}
	req.Header.Set("Accept", "image/avif,image/webp,image/apng,image/svg+xml,image/*,*/*;q=0.8")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("download preview: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("download preview: HTTP status %s", resp.Status)
	}
	temporary, err := os.CreateTemp(c.dir, ".preview-*")
	if err != nil {
		return "", fmt.Errorf("create preview temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return "", fmt.Errorf("secure preview temporary file: %w", err)
	}
	if _, err := io.Copy(temporary, io.LimitReader(resp.Body, maxImageSize+1)); err != nil {
		_ = temporary.Close()
		return "", fmt.Errorf("write preview: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return "", fmt.Errorf("close preview temporary file: %w", err)
	}
	info, err := os.Stat(temporaryPath)
	if err != nil {
		return "", fmt.Errorf("stat downloaded preview: %w", err)
	}
	if info.Size() == 0 {
		return "", fmt.Errorf("downloaded preview is empty")
	}
	if info.Size() > maxImageSize {
		return "", fmt.Errorf("downloaded preview exceeds %d bytes", maxImageSize)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return "", fmt.Errorf("cache preview: %w", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		return "", fmt.Errorf("set preview permissions: %w", err)
	}
	return path, nil
}
