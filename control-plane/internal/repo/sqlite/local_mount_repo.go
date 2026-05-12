package sqlite

import (
	"context"
	"database/sql"
	"time"
)

type LocalMountRepo struct {
	db *sql.DB
}

func NewLocalMountRepo(db *sql.DB) *LocalMountRepo {
	return &LocalMountRepo{db: db}
}

func (r *LocalMountRepo) Enabled(ctx context.Context) (bool, error) {
	var enabled int
	err := r.db.QueryRowContext(ctx, `SELECT enabled FROM local_mount_settings WHERE id = 1`).Scan(&enabled)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return enabled == 1, nil
}

func (r *LocalMountRepo) SetEnabled(ctx context.Context, enabled bool) error {
	_, err := r.db.ExecContext(ctx, `
INSERT INTO local_mount_settings(id, enabled, updated_at)
VALUES (1, ?, ?)
ON CONFLICT(id) DO UPDATE SET enabled = excluded.enabled, updated_at = excluded.updated_at`,
		boolToInt(enabled), formatTime(time.Now().UTC()))
	return err
}
