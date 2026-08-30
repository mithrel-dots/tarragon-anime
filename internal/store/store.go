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

const schemaVersion = 3

type Progress struct {
	MediaID  int
	Episode  int
	Position float64
	Duration float64
	Complete bool
}

type MediaInfo struct {
	ID          int
	Title       string
	PreviewPath string
	Episodes    int
}

type ResumeEntry struct {
	MediaID       int
	Title         string
	PreviewPath   string
	TotalEpisodes int
	Episode       int
	Position      float64
	Duration      float64
	Complete      bool
}

type SyncItem struct {
	ID       int64
	MediaID  int
	Episode  int
	Attempts int
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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin watch progress save: %w", err)
	}
	defer tx.Rollback()

	var writeSeq int64
	if err := tx.QueryRowContext(ctx, `
		UPDATE progress_write_sequence
		SET value = value + 1
		WHERE id = 1
		RETURNING value`).Scan(&writeSeq); err != nil {
		return fmt.Errorf("advance watch progress sequence: %w", err)
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO watch_progress (anilist_id, episode, position, duration, completed, updated_at, write_seq)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(anilist_id, episode) DO UPDATE SET
			position = excluded.position,
			duration = excluded.duration,
			completed = excluded.completed,
			updated_at = excluded.updated_at,
			write_seq = excluded.write_seq`,
		progress.MediaID, progress.Episode, progress.Position, progress.Duration,
		progress.Complete, time.Now().Unix(), writeSeq,
	)
	if err != nil {
		return fmt.Errorf("save watch progress: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit watch progress: %w", err)
	}
	return nil
}

func (s *Store) SaveMedia(ctx context.Context, media MediaInfo) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO media (anilist_id, title, preview_path, episodes, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(anilist_id) DO UPDATE SET
			title = excluded.title,
			preview_path = excluded.preview_path,
			episodes = excluded.episodes,
			updated_at = excluded.updated_at`,
		media.ID, media.Title, media.PreviewPath, media.Episodes, time.Now().Unix(),
	)
	if err != nil {
		return fmt.Errorf("save media %d: %w", media.ID, err)
	}
	return nil
}

// MediaProgress returns the most recently updated episode for a media entry,
// which is the episode a user expects to resume.
func (s *Store) MediaProgress(ctx context.Context, mediaID int) (Progress, bool, error) {
	var progress Progress
	var complete int
	err := s.db.QueryRowContext(ctx, `
		SELECT anilist_id, episode, position, duration, completed
		FROM watch_progress WHERE anilist_id = ?
		ORDER BY write_seq DESC LIMIT 1`,
		mediaID,
	).Scan(&progress.MediaID, &progress.Episode, &progress.Position, &progress.Duration, &complete)
	if errors.Is(err, sql.ErrNoRows) {
		return Progress{}, false, nil
	}
	if err != nil {
		return Progress{}, false, fmt.Errorf("query media progress: %w", err)
	}
	progress.Complete = complete != 0
	return progress, true, nil
}

func (s *Store) ResumeEntries(ctx context.Context, limit int) ([]ResumeEntry, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT w.anilist_id, m.title, m.preview_path, m.episodes,
		       w.episode, w.position, w.duration, w.completed
		FROM watch_progress w
		JOIN media m ON m.anilist_id = w.anilist_id
		WHERE w.rowid = (
			SELECT latest.rowid FROM watch_progress latest
			WHERE latest.anilist_id = w.anilist_id
			ORDER BY latest.write_seq DESC
			LIMIT 1
		)
		ORDER BY w.write_seq DESC
		LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("query resume entries: %w", err)
	}
	defer rows.Close()
	var entries []ResumeEntry
	for rows.Next() {
		var entry ResumeEntry
		var complete int
		if err := rows.Scan(
			&entry.MediaID, &entry.Title, &entry.PreviewPath, &entry.TotalEpisodes,
			&entry.Episode, &entry.Position, &entry.Duration, &complete,
		); err != nil {
			return nil, fmt.Errorf("scan resume entry: %w", err)
		}
		entry.Complete = complete != 0
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read resume entries: %w", err)
	}
	return entries, nil
}

func (s *Store) EnqueueSync(ctx context.Context, mediaID, episode int) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sync_queue (anilist_id, episode, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT(anilist_id, episode) DO UPDATE SET
			updated_at = excluded.updated_at`,
		mediaID, episode, time.Now().Unix(),
	)
	if err != nil {
		return fmt.Errorf("enqueue AniList sync: %w", err)
	}
	return nil
}

func (s *Store) PendingSyncs(ctx context.Context, limit int) ([]SyncItem, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, anilist_id, episode, attempts
		FROM sync_queue ORDER BY updated_at LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("query AniList sync queue: %w", err)
	}
	defer rows.Close()
	var items []SyncItem
	for rows.Next() {
		var item SyncItem
		if err := rows.Scan(&item.ID, &item.MediaID, &item.Episode, &item.Attempts); err != nil {
			return nil, fmt.Errorf("scan AniList sync item: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read AniList sync queue: %w", err)
	}
	return items, nil
}

func (s *Store) DeleteSync(ctx context.Context, id int64) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sync_queue WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete AniList sync item: %w", err)
	}
	return nil
}

func (s *Store) RecordSyncFailure(ctx context.Context, id int64, reason string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE sync_queue
		SET attempts = attempts + 1, last_error = ?, updated_at = ?
		WHERE id = ?`, reason, time.Now().Unix(), id)
	if err != nil {
		return fmt.Errorf("record AniList sync failure: %w", err)
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
	var version int
	if err := tx.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("read SQLite schema version: %w", err)
	}
	if version > schemaVersion {
		return fmt.Errorf("unsupported SQLite schema version %d (latest supported is %d)", version, schemaVersion)
	}
	for version < schemaVersion {
		target := version + 1
		var statements []string
		switch target {
		case 1:
			statements = []string{
				`CREATE TABLE provider_mappings (
					anilist_id INTEGER NOT NULL,
					provider TEXT NOT NULL,
					provider_id TEXT NOT NULL,
					updated_at INTEGER NOT NULL,
					PRIMARY KEY (anilist_id, provider)
				)`,
				`CREATE TABLE watch_progress (
					anilist_id INTEGER NOT NULL,
					episode INTEGER NOT NULL,
					position REAL NOT NULL DEFAULT 0,
					duration REAL NOT NULL DEFAULT 0,
					completed INTEGER NOT NULL DEFAULT 0,
					updated_at INTEGER NOT NULL,
					PRIMARY KEY (anilist_id, episode)
				)`,
			}
		case 2:
			statements = []string{
				`CREATE TABLE media (
					anilist_id INTEGER PRIMARY KEY,
					title TEXT NOT NULL,
					preview_path TEXT NOT NULL DEFAULT '',
					episodes INTEGER NOT NULL DEFAULT 0,
					updated_at INTEGER NOT NULL
				)`,
				`CREATE TABLE sync_queue (
					id INTEGER PRIMARY KEY AUTOINCREMENT,
					anilist_id INTEGER NOT NULL,
					episode INTEGER NOT NULL,
					attempts INTEGER NOT NULL DEFAULT 0,
					last_error TEXT NOT NULL DEFAULT '',
					updated_at INTEGER NOT NULL,
					UNIQUE (anilist_id, episode)
				)`,
			}
		case 3:
			statements = []string{
				`ALTER TABLE watch_progress ADD COLUMN write_seq INTEGER NOT NULL DEFAULT 0`,
				`WITH ranked AS (
					SELECT rowid, ROW_NUMBER() OVER (
						ORDER BY updated_at, episode, anilist_id
					) AS write_seq
					FROM watch_progress
				)
				UPDATE watch_progress
				SET write_seq = (
					SELECT ranked.write_seq FROM ranked
					WHERE ranked.rowid = watch_progress.rowid
				)`,
				`CREATE UNIQUE INDEX watch_progress_write_seq ON watch_progress(write_seq)`,
				`CREATE TABLE progress_write_sequence (
					id INTEGER PRIMARY KEY CHECK (id = 1),
					value INTEGER NOT NULL
				)`,
				`INSERT INTO progress_write_sequence (id, value)
				 SELECT 1, COALESCE(MAX(write_seq), 0) FROM watch_progress`,
			}
		default:
			return fmt.Errorf("no migration for SQLite schema version %d", target)
		}
		for _, statement := range statements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("migrate SQLite schema to version %d: %w", target, err)
			}
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, target)); err != nil {
			return fmt.Errorf("record SQLite schema version %d: %w", target, err)
		}
		version = target
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration: %w", err)
	}
	return nil
}
