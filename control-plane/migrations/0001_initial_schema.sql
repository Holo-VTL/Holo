CREATE TABLE IF NOT EXISTS storage_pools (
  pool_id TEXT PRIMARY KEY,
  name TEXT UNIQUE NOT NULL,
  capacity_bytes BIGINT NOT NULL CHECK (capacity_bytes > 0),
  used_bytes BIGINT NOT NULL DEFAULT 0 CHECK (used_bytes >= 0),
  status TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS virtual_libraries (
  library_id TEXT PRIMARY KEY,
  pool_id TEXT NOT NULL REFERENCES storage_pools(pool_id),
  name TEXT NOT NULL,
  status TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL,
  UNIQUE(pool_id, name)
);

CREATE TABLE IF NOT EXISTS virtual_drives (
  drive_id TEXT PRIMARY KEY,
  library_id TEXT NOT NULL REFERENCES virtual_libraries(library_id),
  slot INT NOT NULL,
  mount_state TEXT NOT NULL,
  mounted_cartridge_id TEXT,
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL,
  UNIQUE(library_id, slot)
);

CREATE TABLE IF NOT EXISTS virtual_cartridges (
  cartridge_id TEXT PRIMARY KEY,
  library_id TEXT NOT NULL REFERENCES virtual_libraries(library_id),
  barcode TEXT UNIQUE NOT NULL,
  capacity_bytes BIGINT NOT NULL CHECK (capacity_bytes > 0),
  used_bytes BIGINT NOT NULL DEFAULT 0 CHECK (used_bytes >= 0),
  lifecycle_state TEXT NOT NULL,
  retention_state TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS audit_events (
  event_id TEXT PRIMARY KEY,
  actor TEXT NOT NULL,
  action TEXT NOT NULL,
  object_type TEXT NOT NULL,
  object_id TEXT NOT NULL,
  result TEXT NOT NULL,
  details JSONB,
  occurred_at TIMESTAMPTZ NOT NULL
);
