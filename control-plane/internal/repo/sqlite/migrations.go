package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var ErrUnsupportedSchemaVersion = errors.New("database schema version is newer than supported")

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
	{
		version: 5,
		sql: `
CREATE TABLE IF NOT EXISTS local_mount_settings (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  enabled INTEGER NOT NULL DEFAULT 0,
  updated_at TEXT NOT NULL
);
`,
	},
	{
		version: 6,
		sql: `
ALTER TABLE virtual_cartridges ADD COLUMN assigned_slot_address INTEGER;
`,
	},
	{
		version: 7,
		sql: `
CREATE UNIQUE INDEX IF NOT EXISTS idx_virtual_drives_drive_library
  ON virtual_drives(drive_id, library_id);

CREATE TABLE IF NOT EXISTS iscsi_chap_credentials (
  credential_id TEXT PRIMARY KEY,
  label TEXT NOT NULL,
  username TEXT NOT NULL,
  mutual_username TEXT NOT NULL DEFAULT '',
  encrypted_secret BLOB NOT NULL,
  version INTEGER NOT NULL CHECK(version > 0),
  created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS library_iscsi_security (
  library_id TEXT PRIMARY KEY,
  auth_mode TEXT,
  credential_id TEXT,
  initiators_json TEXT NOT NULL DEFAULT '[]',
  generation INTEGER NOT NULL CHECK(generation > 0),
  FOREIGN KEY(library_id) REFERENCES virtual_libraries(library_id) ON DELETE CASCADE,
  FOREIGN KEY(credential_id) REFERENCES iscsi_chap_credentials(credential_id) ON DELETE RESTRICT,
  CHECK(auth_mode IS NULL OR auth_mode IN ('none', 'chap', 'mutual_chap')),
  CHECK((auth_mode IN ('chap', 'mutual_chap') AND credential_id IS NOT NULL AND initiators_json <> '[]') OR (auth_mode = 'none' AND credential_id IS NULL) OR (auth_mode IS NULL AND credential_id IS NULL AND initiators_json = '[]'))
);

CREATE TABLE IF NOT EXISTS drive_iscsi_security (
  drive_id TEXT PRIMARY KEY,
  library_id TEXT NOT NULL,
  auth_mode TEXT,
  credential_id TEXT,
  initiators_json TEXT NOT NULL DEFAULT '[]',
  generation INTEGER NOT NULL CHECK(generation > 0),
  FOREIGN KEY(drive_id, library_id) REFERENCES virtual_drives(drive_id, library_id) ON DELETE CASCADE,
  FOREIGN KEY(credential_id) REFERENCES iscsi_chap_credentials(credential_id) ON DELETE RESTRICT,
  CHECK(auth_mode IS NULL OR auth_mode IN ('none', 'chap', 'mutual_chap')),
  CHECK((auth_mode IN ('chap', 'mutual_chap') AND credential_id IS NOT NULL AND initiators_json <> '[]') OR (auth_mode = 'none' AND credential_id IS NULL) OR (auth_mode IS NULL AND credential_id IS NULL AND initiators_json = '[]'))
);

CREATE TABLE IF NOT EXISTS target_iscsi_security (
  target_iqn TEXT PRIMARY KEY,
  library_id TEXT NOT NULL,
  drive_id TEXT,
  device_role TEXT NOT NULL CHECK(device_role IN ('drive', 'changer')),
  administrative_offline INTEGER NOT NULL DEFAULT 0 CHECK(administrative_offline IN (0, 1)),
  auth_mode TEXT,
  credential_id TEXT,
  initiators_json TEXT NOT NULL DEFAULT '[]',
  generation INTEGER NOT NULL CHECK(generation > 0),
  FOREIGN KEY(library_id) REFERENCES virtual_libraries(library_id) ON DELETE CASCADE,
  FOREIGN KEY(drive_id, library_id) REFERENCES virtual_drives(drive_id, library_id) ON DELETE CASCADE,
  FOREIGN KEY(credential_id) REFERENCES iscsi_chap_credentials(credential_id) ON DELETE RESTRICT,
  CHECK((device_role = 'drive' AND drive_id IS NOT NULL) OR (device_role = 'changer' AND drive_id IS NULL)),
  CHECK(auth_mode IS NULL OR auth_mode IN ('none', 'chap', 'mutual_chap')),
  CHECK((auth_mode IN ('chap', 'mutual_chap') AND credential_id IS NOT NULL AND initiators_json <> '[]') OR (auth_mode = 'none' AND credential_id IS NULL) OR (auth_mode IS NULL AND credential_id IS NULL AND initiators_json = '[]'))
);

CREATE TABLE IF NOT EXISTS iscsi_security_snapshots (
  snapshot_id TEXT PRIMARY KEY,
  scope TEXT NOT NULL CHECK(scope IN ('library', 'drive', 'target')),
  owner_id TEXT NOT NULL,
  version INTEGER NOT NULL CHECK(version > 0),
  payload BLOB NOT NULL,
  created_by TEXT NOT NULL,
  created_at TEXT NOT NULL,
  UNIQUE(scope, owner_id, version)
);

CREATE TABLE IF NOT EXISTS iscsi_snapshot_credentials (
  snapshot_id TEXT NOT NULL,
  credential_id TEXT NOT NULL,
  PRIMARY KEY(snapshot_id, credential_id),
  FOREIGN KEY(snapshot_id) REFERENCES iscsi_security_snapshots(snapshot_id) ON DELETE CASCADE,
  FOREIGN KEY(credential_id) REFERENCES iscsi_chap_credentials(credential_id) ON DELETE RESTRICT
);

CREATE INDEX IF NOT EXISTS idx_iscsi_security_snapshot_history
  ON iscsi_security_snapshots(scope, owner_id, version DESC);
`,
	},
	{
		version: 8,
		sql: `
ALTER TABLE target_publications ADD COLUMN security_enforcement TEXT NOT NULL DEFAULT 'unprotected'
  CHECK(security_enforcement IN ('unprotected', 'simulated', 'enforcing', 'blocked', 'offline'));
`,
	},
	{
		version: 9,
		sql: `
ALTER TABLE library_iscsi_security ADD COLUMN restrict_initiators INTEGER NOT NULL DEFAULT 0 CHECK(restrict_initiators IN (0, 1));
ALTER TABLE drive_iscsi_security ADD COLUMN restrict_initiators INTEGER NOT NULL DEFAULT 0 CHECK(restrict_initiators IN (0, 1));
ALTER TABLE target_iscsi_security ADD COLUMN restrict_initiators INTEGER NOT NULL DEFAULT 0 CHECK(restrict_initiators IN (0, 1));
`,
	},
	{
		version: 10,
		sql: `
CREATE TABLE IF NOT EXISTS local_loopback_libraries (
  library_id TEXT PRIMARY KEY,
  target_naa TEXT NOT NULL UNIQUE,
  nexus_naa TEXT NOT NULL UNIQUE,
  tpg_tag INTEGER NOT NULL CHECK(tpg_tag = 1)
);

CREATE TABLE IF NOT EXISTS local_loopback_devices (
  device_key TEXT PRIMARY KEY,
  library_id TEXT NOT NULL,
  kind TEXT NOT NULL CHECK(kind IN ('changer', 'drive')),
  drive_id TEXT,
  lun_index INTEGER NOT NULL,
  identity_ref TEXT NOT NULL UNIQUE,
  backend_ref TEXT NOT NULL UNIQUE,
  state TEXT NOT NULL CHECK(state IN ('active', 'cleanup_pending', 'inactive')),
  FOREIGN KEY(library_id) REFERENCES local_loopback_libraries(library_id) ON DELETE RESTRICT,
  UNIQUE(library_id, lun_index),
  CHECK((kind = 'changer' AND device_key = 'changer:' || library_id AND drive_id IS NULL AND lun_index = 0) OR
        (kind = 'drive' AND drive_id IS NOT NULL AND device_key = 'drive:' || drive_id AND lun_index BETWEEN 1 AND 65535))
);

CREATE INDEX IF NOT EXISTS idx_local_loopback_devices_library
  ON local_loopback_devices(library_id, device_key);
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
	maxVersion := migrations[len(migrations)-1].version
	for version := range applied {
		if version > maxVersion {
			return fmt.Errorf("%w: database version %d, supported maximum %d", ErrUnsupportedSchemaVersion, version, maxVersion)
		}
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
