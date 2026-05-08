package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

type migration struct {
	version int
	sql     string
}

var migrations = []migration{
	{
		version: 1,
		sql: `
CREATE TABLE IF NOT EXISTS virtual_libraries (
  library_id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  status TEXT NOT NULL,
  vendor TEXT NOT NULL DEFAULT '',
  library_type TEXT NOT NULL DEFAULT '',
  drive_type TEXT NOT NULL DEFAULT '',
  drive_count INTEGER NOT NULL DEFAULT 0,
  drive_start_address INTEGER NOT NULL DEFAULT 0,
  slot_count INTEGER NOT NULL DEFAULT 0,
  slot_start_address INTEGER NOT NULL DEFAULT 0,
  ie_port_count INTEGER NOT NULL DEFAULT 0,
  ie_start_address INTEGER NOT NULL DEFAULT 0,
  iqn TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS virtual_drives (
  drive_id TEXT PRIMARY KEY,
  library_id TEXT NOT NULL,
  slot INTEGER NOT NULL,
  iqn TEXT NOT NULL DEFAULT '',
  mount_state TEXT NOT NULL,
  mounted_cartridge_id TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  FOREIGN KEY(library_id) REFERENCES virtual_libraries(library_id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS storage_pools (
  pool_id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  status TEXT NOT NULL,
  warning_threshold_pct INTEGER NOT NULL,
  used_bytes INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS virtual_cartridges (
  cartridge_id TEXT PRIMARY KEY,
  pool_id TEXT NOT NULL,
  library_id TEXT NOT NULL,
  barcode TEXT NOT NULL,
  barcode_key TEXT NOT NULL UNIQUE,
  capacity_bytes INTEGER NOT NULL,
  used_bytes INTEGER NOT NULL DEFAULT 0,
  lifecycle_state TEXT NOT NULL,
  retention_state TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  FOREIGN KEY(pool_id) REFERENCES storage_pools(pool_id) ON DELETE RESTRICT,
  FOREIGN KEY(library_id) REFERENCES virtual_libraries(library_id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS storage_pool_disks (
  device_path TEXT PRIMARY KEY,
  pool_id TEXT NOT NULL,
  size_bytes INTEGER NOT NULL,
  attached_at TEXT NOT NULL,
  FOREIGN KEY(pool_id) REFERENCES storage_pools(pool_id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS access_policies (
  policy_id TEXT PRIMARY KEY,
  scope TEXT NOT NULL,
  subject TEXT NOT NULL,
  permission TEXT NOT NULL,
  effective_from TEXT NOT NULL,
  effective_to TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS retention_policies (
  retention_id TEXT PRIMARY KEY,
  cartridge_id TEXT NOT NULL,
  mode TEXT NOT NULL,
  lock_until TEXT NOT NULL,
  created_by TEXT NOT NULL
);
`,
	},
	{
		version: 2,
		sql: `

CREATE TABLE IF NOT EXISTS target_publications (
  publication_id TEXT PRIMARY KEY,
  pool_id TEXT NOT NULL,
  library_id TEXT NOT NULL,
  drive_id TEXT NOT NULL,
  cartridge_id TEXT NOT NULL,
  target_iqn TEXT NOT NULL,
  device_role TEXT NOT NULL,
  device_profile TEXT NOT NULL DEFAULT '',
  drive_profile TEXT NOT NULL DEFAULT '',
  portal TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL,
  last_error TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  FOREIGN KEY(pool_id) REFERENCES storage_pools(pool_id) ON DELETE RESTRICT,
  FOREIGN KEY(library_id) REFERENCES virtual_libraries(library_id) ON DELETE CASCADE,
  FOREIGN KEY(drive_id) REFERENCES virtual_drives(drive_id) ON DELETE CASCADE,
  FOREIGN KEY(cartridge_id) REFERENCES virtual_cartridges(cartridge_id) ON DELETE CASCADE
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_target_publications_active_iqn
  ON target_publications(target_iqn)
  WHERE state IN ('creating', 'ready');

CREATE INDEX IF NOT EXISTS idx_target_publications_state
  ON target_publications(state);

CREATE TABLE IF NOT EXISTS validation_runs (
  validation_id TEXT PRIMARY KEY,
  publication_id TEXT NOT NULL,
  scenario TEXT NOT NULL,
  status TEXT NOT NULL,
  mode TEXT NOT NULL,
  bytes_written INTEGER NOT NULL,
  bytes_read INTEGER NOT NULL,
  write_digest TEXT NOT NULL DEFAULT '',
  read_digest TEXT NOT NULL DEFAULT '',
  evidence_path TEXT NOT NULL DEFAULT '',
  started_at TEXT NOT NULL,
  finished_at TEXT NOT NULL DEFAULT '',
  FOREIGN KEY(publication_id) REFERENCES target_publications(publication_id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_validation_runs_publication
  ON validation_runs(publication_id, started_at);
`,
	},
	{
		version: 3,
		sql: `
ALTER TABLE virtual_libraries ADD COLUMN compression_enabled INTEGER NOT NULL DEFAULT 1;
ALTER TABLE virtual_libraries ADD COLUMN dedup_enabled INTEGER NOT NULL DEFAULT 1;
ALTER TABLE target_publications ADD COLUMN compression_enabled INTEGER NOT NULL DEFAULT 1;
ALTER TABLE target_publications ADD COLUMN dedup_enabled INTEGER NOT NULL DEFAULT 1;
`,
	},
	{
		version: 4,
		sql: `
CREATE TABLE IF NOT EXISTS destroyed_cartridge_barcodes (
  barcode_key TEXT PRIMARY KEY,
  barcode TEXT NOT NULL,
  cartridge_id TEXT NOT NULL,
  actor TEXT NOT NULL,
  destroyed_at TEXT NOT NULL
);
`,
	},
}

func Migrate(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		return err
	}
	applied, err := appliedVersions(ctx, db)
	if err != nil {
		return err
	}
	for _, m := range migrations {
		if applied[m.version] {
			continue
		}
		if err := applyMigration(ctx, db, m); err != nil {
			return err
		}
	}
	return nil
}

func appliedVersions(ctx context.Context, db *sql.DB) (map[int]bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	versions := make(map[int]bool)
	for rows.Next() {
		var version int
		if err := rows.Scan(&version); err != nil {
			return nil, err
		}
		versions[version] = true
	}
	return versions, rows.Err()
}

func applyMigration(ctx context.Context, db *sql.DB, m migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("apply migration %d: %w", m.version, err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at) VALUES (?, ?)`, m.version, formatTime(time.Now().UTC())); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("record migration %d: %w", m.version, err)
	}
	return tx.Commit()
}
