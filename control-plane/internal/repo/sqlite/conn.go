package sqlite

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/Holo-VTL/Holo/control-plane/internal/metadata"

	_ "modernc.org/sqlite"
)

func Open(ctx context.Context, dsn string) (*sql.DB, error) {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		dsn = metadata.DefaultDSN
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One open connection keeps SQLite writes serialized and makes repository
	// preflight checks deterministic before UNIQUE constraints enforce them.
	db.SetMaxOpenConns(1)
	if err := configure(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := Migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// beginSerializedWriteTx takes SQLite's writer reservation before repository
// projection checks. This keeps the read/check/write sequence atomic across
// independent control-plane processes without adding a projection table.
func beginSerializedWriteTx(ctx context.Context, db *sql.DB) (*sql.Tx, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE schema_migrations SET applied_at=applied_at WHERE version=(SELECT MIN(version) FROM schema_migrations)`); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	return tx, nil
}

func configure(ctx context.Context, db *sql.DB) error {
	pragmas := []string{
		"PRAGMA busy_timeout = 5000",
		"PRAGMA foreign_keys = ON",
		"PRAGMA journal_mode = WAL",
	}
	for _, stmt := range pragmas {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func parseTime(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
