package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

type Progress struct {
	MediaID  int
	Episode  int
	Position float64
	Duration float64
	Complete bool
}

func StatePath() (string, error) {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "tarragon", "anime.db"), nil
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open SQLite: %w", err)
	}
	db.SetMaxOpenConns(1)
	store := &Store{db: db}
	if err := store.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("secure SQLite database: %w", err)
	}
	return store, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) ProviderMapping(ctx context.Context, mediaID int, provider string) (string, bool, error) {
	var providerID string
	err := s.db.QueryRowContext(ctx,
		`SELECT provider_id FROM provider_mappings WHERE anilist_id = ? AND provider = ?`,
		mediaID, provider,
	).Scan(&providerID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("query provider mapping: %w", err)
	}
	return providerID, true, nil
}

func (s *Store) SaveProviderMapping(ctx context.Context, mediaID int, provider, providerID string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO provider_mappings (anilist_id, provider, provider_id, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(anilist_id, provider) DO UPDATE SET
			provider_id = excluded.provider_id,
			updated_at = excluded.updated_at`,
		mediaID, provider, providerID, time.Now().Unix(),
	)
	if err != nil {
		return fmt.Errorf("save provider mapping: %w", err)
	}
	return nil
}

func (s *Store) Progress(ctx context.Context, mediaID, episode int) (Progress, bool, error) {
	var progress Progress
	var complete int
	err := s.db.QueryRowContext(ctx, `
		SELECT anilist_id, episode, position, duration, completed
		FROM watch_progress WHERE anilist_id = ? AND episode = ?`,
		mediaID, episode,
	).Scan(&progress.MediaID, &progress.Episode, &progress.Position, &progress.Duration, &complete)
	if errors.Is(err, sql.ErrNoRows) {
		return Progress{}, false, nil
	}
	if err != nil {
		return Progress{}, false, fmt.Errorf("query watch progress: %w", err)
	}
	progress.Complete = complete != 0
	return progress, true, nil
}

func (s *Store) SaveProgress(ctx context.Context, progress Progress) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO watch_progress (anilist_id, episode, position, duration, completed, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(anilist_id, episode) DO UPDATE SET
			position = excluded.position,
			duration = excluded.duration,
			completed = excluded.completed,
			updated_at = excluded.updated_at`,
		progress.MediaID, progress.Episode, progress.Position, progress.Duration,
		progress.Complete, time.Now().Unix(),
	)
	if err != nil {
		return fmt.Errorf("save watch progress: %w", err)
	}
	return nil
}

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `PRAGMA journal_mode = WAL`); err != nil {
		return fmt.Errorf("enable SQLite WAL: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `PRAGMA busy_timeout = 5000`); err != nil {
		return fmt.Errorf("set SQLite busy timeout: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration: %w", err)
	}
	defer tx.Rollback()
	statements := []string{
		`CREATE TABLE IF NOT EXISTS provider_mappings (
			anilist_id INTEGER NOT NULL,
			provider TEXT NOT NULL,
			provider_id TEXT NOT NULL,
			updated_at INTEGER NOT NULL,
			PRIMARY KEY (anilist_id, provider)
		)`,
		`CREATE TABLE IF NOT EXISTS watch_progress (
			anilist_id INTEGER NOT NULL,
			episode INTEGER NOT NULL,
			position REAL NOT NULL DEFAULT 0,
			duration REAL NOT NULL DEFAULT 0,
			completed INTEGER NOT NULL DEFAULT 0,
			updated_at INTEGER NOT NULL,
			PRIMARY KEY (anilist_id, episode)
		)`,
		`PRAGMA user_version = 1`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("apply migration: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration: %w", err)
	}
	return nil
}
