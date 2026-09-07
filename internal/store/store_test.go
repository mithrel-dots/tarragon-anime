package store

import (
	"database/sql"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestStorePersistsMappingsAndProgress(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "anime.db")
	state, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.SaveProviderMapping(t.Context(), 154587, "allanime", "show-1"); err != nil {
		t.Fatal(err)
	}
	wantProgress := Progress{MediaID: 154587, Episode: 4, Position: 317.5, Duration: 1440, Complete: false}
	if err := state.SaveProgress(t.Context(), wantProgress); err != nil {
		t.Fatal(err)
	}
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}

	state, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	providerID, found, err := state.ProviderMapping(t.Context(), 154587, "allanime")
	if err != nil {
		t.Fatal(err)
	}
	if !found || providerID != "show-1" {
		t.Fatalf("ProviderMapping() = %q, %v", providerID, found)
	}
	progress, found, err := state.Progress(t.Context(), 154587, 4)
	if err != nil {
		t.Fatal(err)
	}
	if !found || progress != wantProgress {
		t.Fatalf("Progress() = %#v, %v, want %#v", progress, found, wantProgress)
	}
}

func TestStoreResumeEntriesAndSyncQueue(t *testing.T) {
	state, err := Open(filepath.Join(t.TempDir(), "anime.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	if err := state.SaveMedia(t.Context(), MediaInfo{ID: 1, Title: "Frieren", PreviewPath: "/tmp/1.jpg", Episodes: 28}); err != nil {
		t.Fatal(err)
	}
	if err := state.SaveProgress(t.Context(), Progress{MediaID: 1, Episode: 3, Position: 120, Duration: 1400}); err != nil {
		t.Fatal(err)
	}
	if err := state.SaveProgress(t.Context(), Progress{MediaID: 1, Episode: 4, Position: 30, Duration: 1400}); err != nil {
		t.Fatal(err)
	}
	entries, err := state.ResumeEntries(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Episode != 4 || entries[0].Title != "Frieren" || entries[0].TotalEpisodes != 28 {
		t.Fatalf("ResumeEntries() = %#v", entries)
	}
	progress, found, err := state.MediaProgress(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if !found || progress.Episode != 4 {
		t.Fatalf("MediaProgress() = %#v, %v", progress, found)
	}

	if err := state.EnqueueSync(t.Context(), 1, 4); err != nil {
		t.Fatal(err)
	}
	if err := state.EnqueueSync(t.Context(), 1, 4); err != nil {
		t.Fatal(err)
	}
	items, err := state.PendingSyncs(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Episode != 4 {
		t.Fatalf("PendingSyncs() = %#v", items)
	}
	if err := state.RecordSyncFailure(t.Context(), items[0].ID, "offline"); err != nil {
		t.Fatal(err)
	}
	items, err = state.PendingSyncs(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Attempts != 1 {
		t.Fatalf("PendingSyncs() after failure = %#v", items)
	}
	if err := state.DeleteSync(t.Context(), items[0].ID); err != nil {
		t.Fatal(err)
	}
	items, err = state.PendingSyncs(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("PendingSyncs() after delete = %#v", items)
	}
}

func TestStoreCachedMediaLookup(t *testing.T) {
	state, err := Open(filepath.Join(t.TempDir(), "anime.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	if err := state.SaveMedia(t.Context(), MediaInfo{ID: 154587, Title: "Frieren", PreviewPath: "/tmp/frieren.jpg", Episodes: 28}); err != nil {
		t.Fatal(err)
	}

	media, found, err := state.CachedMedia(t.Context(), 154587)
	if err != nil || !found || media.Title != "Frieren" || media.Episodes != 28 {
		t.Fatalf("CachedMedia() = %#v, %v, %v", media, found, err)
	}
	items, err := state.SearchMedia(t.Context(), "rieren")
	if err != nil || len(items) != 1 || items[0].ID != 154587 {
		t.Fatalf("SearchMedia() = %#v, %v", items, err)
	}
}

func TestStoreUpdatesProgress(t *testing.T) {
	state, err := Open(filepath.Join(t.TempDir(), "anime.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	progress := Progress{MediaID: 1, Episode: 2, Position: 10, Duration: 20}
	if err := state.SaveProgress(t.Context(), progress); err != nil {
		t.Fatal(err)
	}
	progress.Position = 20
	progress.Complete = true
	if err := state.SaveProgress(t.Context(), progress); err != nil {
		t.Fatal(err)
	}
	got, found, err := state.Progress(t.Context(), 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !found || got != progress {
		t.Fatalf("Progress() = %#v, %v, want %#v", got, found, progress)
	}
}

func TestProgressRecencyFollowsWrites(t *testing.T) {
	state, err := Open(filepath.Join(t.TempDir(), "anime.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	for _, media := range []MediaInfo{
		{ID: 1, Title: "First"},
		{ID: 2, Title: "Second"},
	} {
		if err := state.SaveMedia(t.Context(), media); err != nil {
			t.Fatal(err)
		}
	}
	for _, progress := range []Progress{
		{MediaID: 1, Episode: 2, Position: 10},
		{MediaID: 1, Episode: 8, Position: 20},
		{MediaID: 2, Episode: 3, Position: 30},
		{MediaID: 1, Episode: 2, Position: 40},
	} {
		if err := state.SaveProgress(t.Context(), progress); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := state.db.ExecContext(t.Context(), `UPDATE watch_progress SET updated_at = 100`); err != nil {
		t.Fatal(err)
	}

	progress, found, err := state.MediaProgress(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if !found || progress.Episode != 2 || progress.Position != 40 {
		t.Fatalf("MediaProgress() = %#v, %v, want last-written episode 2", progress, found)
	}
	entries, err := state.ResumeEntries(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].MediaID != 1 || entries[0].Episode != 2 || entries[1].MediaID != 2 {
		t.Fatalf("ResumeEntries() = %#v, want media 1 episode 2 followed by media 2", entries)
	}

	var lowerSeq, higherSeq int64
	if err := state.db.QueryRowContext(t.Context(), `
		SELECT lower.write_seq, higher.write_seq
		FROM watch_progress lower, watch_progress higher
		WHERE lower.anilist_id = 1 AND lower.episode = 2
		  AND higher.anilist_id = 1 AND higher.episode = 8`).Scan(&lowerSeq, &higherSeq); err != nil {
		t.Fatal(err)
	}
	if lowerSeq <= higherSeq {
		t.Fatalf("write sequences = lower %d, higher %d; lower episode was written last", lowerSeq, higherSeq)
	}
}

func TestMigrateV1PreservesData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "anime.db")
	db := createLegacyDatabase(t, path, 1)
	mustExec(t, db, `INSERT INTO provider_mappings VALUES (10, 'allanime', 'show-10', 11)`)
	mustExec(t, db, `INSERT INTO watch_progress VALUES (10, 4, 12.5, 24, 1, 12)`)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	state, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	if got := databaseVersion(t, state.db); got != schemaVersion {
		t.Fatalf("user_version = %d, want %d", got, schemaVersion)
	}
	providerID, found, err := state.ProviderMapping(t.Context(), 10, "allanime")
	if err != nil {
		t.Fatal(err)
	}
	if !found || providerID != "show-10" {
		t.Fatalf("ProviderMapping() = %q, %v", providerID, found)
	}
	progress, found, err := state.Progress(t.Context(), 10, 4)
	if err != nil {
		t.Fatal(err)
	}
	want := Progress{MediaID: 10, Episode: 4, Position: 12.5, Duration: 24, Complete: true}
	if !found || progress != want {
		t.Fatalf("Progress() = %#v, %v, want %#v", progress, found, want)
	}
	assertColumnExists(t, state.db, "watch_progress", "write_seq")
	assertTableExists(t, state.db, "media")
	assertTableExists(t, state.db, "sync_queue")
	assertTableExists(t, state.db, "progress_write_sequence")
}

func TestMigrateDeployedV2PreservesData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "anime.db")
	db := createLegacyDatabase(t, path, 2)
	mustExec(t, db, `INSERT INTO provider_mappings VALUES (20, 'allanime', 'show-20', 100)`)
	mustExec(t, db, `INSERT INTO watch_progress VALUES (20, 2, 5, 25, 0, 100)`)
	mustExec(t, db, `INSERT INTO watch_progress VALUES (20, 7, 15, 25, 1, 200)`)
	mustExec(t, db, `INSERT INTO media VALUES (20, 'Migrated', '/preview.jpg', 12, 300)`)
	mustExec(t, db, `INSERT INTO sync_queue (anilist_id, episode, attempts, last_error, updated_at)
		VALUES (20, 7, 2, 'offline', 400)`)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	state, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := databaseVersion(t, state.db); got != schemaVersion {
		t.Fatalf("user_version = %d, want %d", got, schemaVersion)
	}
	progress, found, err := state.MediaProgress(t.Context(), 20)
	if err != nil {
		t.Fatal(err)
	}
	if !found || progress.Episode != 7 || !progress.Complete {
		t.Fatalf("MediaProgress() = %#v, %v", progress, found)
	}
	entries, err := state.ResumeEntries(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Title != "Migrated" || entries[0].Episode != 7 {
		t.Fatalf("ResumeEntries() = %#v", entries)
	}
	items, err := state.PendingSyncs(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Episode != 7 || items[0].Attempts != 2 {
		t.Fatalf("PendingSyncs() = %#v", items)
	}
	var lastError string
	if err := state.db.QueryRowContext(t.Context(), `SELECT last_error FROM sync_queue`).Scan(&lastError); err != nil {
		t.Fatal(err)
	}
	if lastError != "offline" {
		t.Fatalf("last_error = %q, want offline", lastError)
	}
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}

	state, err = Open(path)
	if err != nil {
		t.Fatalf("reopen current schema: %v", err)
	}
	defer state.Close()
	if got := databaseVersion(t, state.db); got != schemaVersion {
		t.Fatalf("user_version after reopen = %d, want %d", got, schemaVersion)
	}
}

func TestMigratePopulatedUnversionedDatabase(t *testing.T) {
	for _, version := range []int{1, 2} {
		t.Run("v"+strconv.Itoa(version), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "anime.db")
			db := createLegacyDatabase(t, path, version)
			mustExec(t, db, `INSERT INTO provider_mappings VALUES (25, 'allanime', 'show-25', 100)`)
			mustExec(t, db, `INSERT INTO watch_progress VALUES (25, 3, 15, 25, 0, 200)`)
			if version == 2 {
				mustExec(t, db, `INSERT INTO media VALUES (25, 'Unversioned', '/preview.jpg', 12, 300)`)
				mustExec(t, db, `INSERT INTO sync_queue (anilist_id, episode, attempts, last_error, updated_at)
					VALUES (25, 3, 1, 'retry', 400)`)
			}
			mustExec(t, db, `PRAGMA user_version = 0`)
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}

			state, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer state.Close()
			if got := databaseVersion(t, state.db); got != schemaVersion {
				t.Fatalf("user_version = %d, want %d", got, schemaVersion)
			}
			providerID, found, err := state.ProviderMapping(t.Context(), 25, "allanime")
			if err != nil {
				t.Fatal(err)
			}
			if !found || providerID != "show-25" {
				t.Fatalf("ProviderMapping() = %q, %v", providerID, found)
			}
			progress, found, err := state.Progress(t.Context(), 25, 3)
			if err != nil {
				t.Fatal(err)
			}
			if !found || progress.Position != 15 {
				t.Fatalf("Progress() = %#v, %v", progress, found)
			}
			if version == 2 {
				entries, err := state.ResumeEntries(t.Context(), 10)
				if err != nil {
					t.Fatal(err)
				}
				if len(entries) != 1 || entries[0].Title != "Unversioned" || entries[0].Episode != 3 {
					t.Fatalf("ResumeEntries() = %#v", entries)
				}
				items, err := state.PendingSyncs(t.Context(), 10)
				if err != nil {
					t.Fatal(err)
				}
				if len(items) != 1 || items[0].Attempts != 1 {
					t.Fatalf("PendingSyncs() = %#v", items)
				}
			}
		})
	}
}

func TestMigrateEmptyUnversionedDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "anime.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.PingContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	state, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	if got := databaseVersion(t, state.db); got != schemaVersion {
		t.Fatalf("user_version = %d, want %d", got, schemaVersion)
	}
	for _, table := range []string{"provider_mappings", "watch_progress", "media", "sync_queue", "progress_write_sequence"} {
		assertTableExists(t, state.db, table)
	}
}

func TestRejectsMalformedPopulatedUnversionedDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "anime.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `CREATE TABLE provider_mappings (
		anilist_id INTEGER NOT NULL,
		provider TEXT NOT NULL,
		provider_id TEXT NOT NULL,
		updated_at INTEGER NOT NULL,
		PRIMARY KEY (anilist_id, provider)
	)`)
	mustExec(t, db, `CREATE TABLE watch_progress (
		anilist_id INTEGER NOT NULL,
		episode INTEGER NOT NULL,
		position REAL NOT NULL DEFAULT 0,
		PRIMARY KEY (anilist_id, episode)
	)`)
	mustExec(t, db, `INSERT INTO watch_progress VALUES (31, 4, 17)`)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "unsupported populated unversioned SQLite schema") {
		t.Fatalf("Open() error = %v, want unsupported populated schema", err)
	}
	db, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if got := databaseVersion(t, db); got != 0 {
		t.Fatalf("user_version = %d after rejected inference, want 0", got)
	}
	var count int
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM watch_progress`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("watch_progress row count = %d after rejection, want 1", count)
	}
}

func TestMigrationFailureDoesNotAdvanceVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "anime.db")
	db := createLegacyDatabase(t, path, 1)
	mustExec(t, db, `CREATE TABLE media (wrong_column INTEGER)`)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := Open(path); err == nil {
		t.Fatal("Open() succeeded for an invalid v1 schema")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if got := databaseVersion(t, db); got != 1 {
		t.Fatalf("user_version = %d after failed migration, want 1", got)
	}
	assertTableMissing(t, db, "sync_queue")
}

func TestOpenRejectsNewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "anime.db")
	state, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.SaveProgress(t.Context(), Progress{MediaID: 30, Episode: 1, Position: 9}); err != nil {
		t.Fatal(err)
	}
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `PRAGMA user_version = 4`)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "unsupported SQLite schema version 4") {
		t.Fatalf("Open() error = %v, want unsupported newer schema", err)
	}

	db, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if got := databaseVersion(t, db); got != 4 {
		t.Fatalf("user_version = %d after rejected open, want 4", got)
	}
	var position float64
	if err := db.QueryRowContext(t.Context(), `SELECT position FROM watch_progress WHERE anilist_id = 30`).Scan(&position); err != nil {
		t.Fatal(err)
	}
	if position != 9 {
		t.Fatalf("preserved position = %v, want 9", position)
	}
}

func createLegacyDatabase(t *testing.T, path string, version int) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	statements := []string{
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
	if version >= 2 {
		statements = append(statements,
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
		)
	}
	for _, statement := range statements {
		mustExec(t, db, statement)
	}
	mustExec(t, db, `PRAGMA user_version = `+strconv.Itoa(version))
	return db
}

func mustExec(t *testing.T, db *sql.DB, statement string) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(), statement); err != nil {
		t.Fatalf("execute %q: %v", statement, err)
	}
}

func databaseVersion(t *testing.T, db *sql.DB) int {
	t.Helper()
	var version int
	if err := db.QueryRowContext(t.Context(), `PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	return version
}

func assertColumnExists(t *testing.T, db *sql.DB, table, column string) {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), `PRAGMA table_info(`+table+`)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		if name == column {
			return
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	t.Fatalf("column %s.%s does not exist", table, column)
}

func assertTableExists(t *testing.T, db *sql.DB, table string) {
	t.Helper()
	var count int
	if err := db.QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM sqlite_schema WHERE type = 'table' AND name = ?`, table).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("table %s does not exist", table)
	}
}

func assertTableMissing(t *testing.T, db *sql.DB, table string) {
	t.Helper()
	var count int
	if err := db.QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM sqlite_schema WHERE type = 'table' AND name = ?`, table).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("table %s exists after rolled-back migration", table)
	}
}
