package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
	sqlitedriver "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

type CoreResourcesRepo struct {
	db *sql.DB
}

func NewCoreResourcesRepo(db *sql.DB) *CoreResourcesRepo {
	return &CoreResourcesRepo{db: db}
}

func (r *CoreResourcesRepo) CreateLibrary(ctx context.Context, library *domain.VirtualLibrary) error {
	if library == nil {
		return domain.ErrInvalidInput
	}
	_, err := r.db.ExecContext(ctx, `
INSERT INTO virtual_libraries (
  library_id, name, status, vendor, library_type, drive_type, drive_count,
  drive_start_address, slot_count, slot_start_address, ie_port_count,
  ie_start_address, iqn, compression_enabled, dedup_enabled, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		library.LibraryID,
		library.Name,
		string(library.Status),
		library.Vendor,
		library.LibraryType,
		library.DriveType,
		library.DriveCount,
		library.DriveStartAddress,
		library.SlotCount,
		library.SlotStartAddress,
		library.IEPortCount,
		library.IEStartAddress,
		library.IQN,
		boolToInt(library.CompressionEnabled),
		boolToInt(library.DedupEnabled),
		formatTime(library.CreatedAt),
		formatTime(library.UpdatedAt),
	)
	if isSQLiteConstraint(err) {
		return domain.ErrConflict
	}
	return err
}

func (r *CoreResourcesRepo) SaveLibrary(ctx context.Context, library *domain.VirtualLibrary) error {
	if library == nil {
		return domain.ErrInvalidInput
	}
	_, err := r.db.ExecContext(ctx, `
INSERT INTO virtual_libraries (
  library_id, name, status, vendor, library_type, drive_type, drive_count,
  drive_start_address, slot_count, slot_start_address, ie_port_count,
  ie_start_address, iqn, compression_enabled, dedup_enabled, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(library_id) DO UPDATE SET
  name=excluded.name,
  status=excluded.status,
  vendor=excluded.vendor,
  library_type=excluded.library_type,
  drive_type=excluded.drive_type,
  drive_count=excluded.drive_count,
  drive_start_address=excluded.drive_start_address,
  slot_count=excluded.slot_count,
  slot_start_address=excluded.slot_start_address,
  ie_port_count=excluded.ie_port_count,
  ie_start_address=excluded.ie_start_address,
  iqn=excluded.iqn,
  compression_enabled=excluded.compression_enabled,
  dedup_enabled=excluded.dedup_enabled,
  created_at=excluded.created_at,
  updated_at=excluded.updated_at`,
		library.LibraryID,
		library.Name,
		string(library.Status),
		library.Vendor,
		library.LibraryType,
		library.DriveType,
		library.DriveCount,
		library.DriveStartAddress,
		library.SlotCount,
		library.SlotStartAddress,
		library.IEPortCount,
		library.IEStartAddress,
		library.IQN,
		boolToInt(library.CompressionEnabled),
		boolToInt(library.DedupEnabled),
		formatTime(library.CreatedAt),
		formatTime(library.UpdatedAt),
	)
	return err
}

func (r *CoreResourcesRepo) CreateDrive(ctx context.Context, drive *domain.VirtualDrive) error {
	if drive == nil {
		return domain.ErrInvalidInput
	}
	_, err := r.db.ExecContext(ctx, `
INSERT INTO virtual_drives (
  drive_id, library_id, slot, iqn, mount_state, mounted_cartridge_id, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		drive.DriveID,
		drive.LibraryID,
		drive.Slot,
		drive.IQN,
		string(drive.MountState),
		drive.MountedCartridgeID,
		formatTime(drive.CreatedAt),
		formatTime(drive.UpdatedAt),
	)
	if isSQLiteConstraint(err) {
		return domain.ErrConflict
	}
	return err
}

func (r *CoreResourcesRepo) SaveDrive(ctx context.Context, drive *domain.VirtualDrive) error {
	if drive == nil {
		return domain.ErrInvalidInput
	}
	_, err := r.db.ExecContext(ctx, `
INSERT INTO virtual_drives (
  drive_id, library_id, slot, iqn, mount_state, mounted_cartridge_id, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(drive_id) DO UPDATE SET
  library_id=excluded.library_id,
  slot=excluded.slot,
  iqn=excluded.iqn,
  mount_state=excluded.mount_state,
  mounted_cartridge_id=excluded.mounted_cartridge_id,
  created_at=excluded.created_at,
  updated_at=excluded.updated_at`,
		drive.DriveID,
		drive.LibraryID,
		drive.Slot,
		drive.IQN,
		string(drive.MountState),
		drive.MountedCartridgeID,
		formatTime(drive.CreatedAt),
		formatTime(drive.UpdatedAt),
	)
	return err
}

func (r *CoreResourcesRepo) CreateCartridge(ctx context.Context, cartridge *domain.VirtualCartridge) error {
	if cartridge == nil {
		return domain.ErrInvalidInput
	}
	barcodeKey := normalizeBarcodeKey(cartridge.Barcode)
	destroyed, err := r.isBarcodeDestroyed(ctx, barcodeKey)
	if err != nil {
		return err
	}
	if destroyed {
		return domain.ErrConflict
	}
	_, err = r.db.ExecContext(ctx, `
INSERT INTO virtual_cartridges (
  cartridge_id, pool_id, library_id, barcode, barcode_key, capacity_bytes, used_bytes,
  lifecycle_state, retention_state, assigned_slot_address, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		cartridge.CartridgeID,
		cartridge.PoolID,
		cartridge.LibraryID,
		cartridge.Barcode,
		barcodeKey,
		cartridge.CapacityBytes,
		cartridge.UsedBytes,
		string(cartridge.LifecycleState),
		string(cartridge.RetentionState),
		nullableInt(cartridge.AssignedSlotAddress),
		formatTime(cartridge.CreatedAt),
		formatTime(cartridge.UpdatedAt),
	)
	if isSQLiteConstraint(err) {
		return domain.ErrConflict
	}
	return err
}

func (r *CoreResourcesRepo) SaveCartridge(ctx context.Context, cartridge *domain.VirtualCartridge) error {
	if cartridge == nil {
		return domain.ErrInvalidInput
	}
	barcodeKey := normalizeBarcodeKey(cartridge.Barcode)
	destroyed, err := r.isBarcodeDestroyed(ctx, barcodeKey)
	if err != nil {
		return err
	}
	if destroyed {
		return domain.ErrConflict
	}
	// barcode_key is still protected by a UNIQUE constraint; this precheck only
	// returns a predictable domain error before the DB constraint catches it.
	var existingID string
	err = r.db.QueryRowContext(ctx, `SELECT cartridge_id FROM virtual_cartridges WHERE barcode_key = ? AND cartridge_id <> ?`, barcodeKey, cartridge.CartridgeID).Scan(&existingID)
	if err == nil {
		return domain.ErrConflict
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = r.db.ExecContext(ctx, `
INSERT INTO virtual_cartridges (
  cartridge_id, pool_id, library_id, barcode, barcode_key, capacity_bytes, used_bytes,
  lifecycle_state, retention_state, assigned_slot_address, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(cartridge_id) DO UPDATE SET
  pool_id=excluded.pool_id,
  library_id=excluded.library_id,
  barcode=excluded.barcode,
  barcode_key=excluded.barcode_key,
  capacity_bytes=excluded.capacity_bytes,
  used_bytes=excluded.used_bytes,
  lifecycle_state=excluded.lifecycle_state,
  retention_state=excluded.retention_state,
  assigned_slot_address=excluded.assigned_slot_address,
  created_at=excluded.created_at,
  updated_at=excluded.updated_at`,
		cartridge.CartridgeID,
		cartridge.PoolID,
		cartridge.LibraryID,
		cartridge.Barcode,
		barcodeKey,
		cartridge.CapacityBytes,
		cartridge.UsedBytes,
		string(cartridge.LifecycleState),
		string(cartridge.RetentionState),
		nullableInt(cartridge.AssignedSlotAddress),
		formatTime(cartridge.CreatedAt),
		formatTime(cartridge.UpdatedAt),
	)
	if isSQLiteConstraint(err) {
		return domain.ErrConflict
	}
	return err
}

func (r *CoreResourcesRepo) DeleteCartridge(ctx context.Context, cartridgeID string) error {
	return deleteByID(ctx, r.db, `DELETE FROM virtual_cartridges WHERE cartridge_id = ?`, cartridgeID)
}

func (r *CoreResourcesRepo) RetireCartridgeBarcode(ctx context.Context, barcode, cartridgeID, actor string) error {
	barcodeKey := normalizeBarcodeKey(barcode)
	if barcodeKey == "" || strings.TrimSpace(cartridgeID) == "" {
		return domain.ErrInvalidInput
	}
	_, err := r.db.ExecContext(ctx, `
INSERT INTO destroyed_cartridge_barcodes (barcode_key, barcode, cartridge_id, actor, destroyed_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(barcode_key) DO NOTHING`,
		barcodeKey,
		strings.TrimSpace(barcode),
		strings.TrimSpace(cartridgeID),
		strings.TrimSpace(actor),
		formatTime(time.Now().UTC()),
	)
	if isSQLiteConstraint(err) {
		return domain.ErrConflict
	}
	return err
}

func (r *CoreResourcesRepo) DestroyCartridge(ctx context.Context, cartridgeID, barcode, actor string) error {
	barcodeKey := normalizeBarcodeKey(barcode)
	cartridgeID = strings.TrimSpace(cartridgeID)
	if barcodeKey == "" || cartridgeID == "" {
		return domain.ErrInvalidInput
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO destroyed_cartridge_barcodes (barcode_key, barcode, cartridge_id, actor, destroyed_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(barcode_key) DO NOTHING`,
		barcodeKey,
		strings.TrimSpace(barcode),
		cartridgeID,
		strings.TrimSpace(actor),
		formatTime(time.Now().UTC()),
	); err != nil {
		_ = tx.Rollback()
		if isSQLiteConstraint(err) {
			return domain.ErrConflict
		}
		return err
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM virtual_cartridges WHERE cartridge_id = ?`, cartridgeID)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	if affected == 0 {
		_ = tx.Rollback()
		return domain.ErrNotFound
	}
	return tx.Commit()
}

func (r *CoreResourcesRepo) ListRetiredCartridgeBarcodes(ctx context.Context) []string {
	rows, err := r.db.QueryContext(ctx, `SELECT barcode FROM destroyed_cartridge_barcodes ORDER BY barcode_key`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := make([]string, 0)
	for rows.Next() {
		var barcode string
		if err := rows.Scan(&barcode); err == nil {
			out = append(out, barcode)
		}
	}
	return out
}

func (r *CoreResourcesRepo) DeleteDrive(ctx context.Context, driveID string) error {
	return deleteByID(ctx, r.db, `DELETE FROM virtual_drives WHERE drive_id = ?`, driveID)
}

func (r *CoreResourcesRepo) DeleteLibrary(ctx context.Context, libraryID string) error {
	return deleteByID(ctx, r.db, `DELETE FROM virtual_libraries WHERE library_id = ?`, libraryID)
}

func (r *CoreResourcesRepo) FindLibrary(ctx context.Context, libraryID string) (*domain.VirtualLibrary, error) {
	row := r.db.QueryRowContext(ctx, `
SELECT library_id, name, status, vendor, library_type, drive_type, drive_count,
       drive_start_address, slot_count, slot_start_address, ie_port_count,
       ie_start_address, iqn, compression_enabled, dedup_enabled, created_at, updated_at
FROM virtual_libraries WHERE library_id = ?`, strings.TrimSpace(libraryID))
	return scanLibrary(row)
}

func (r *CoreResourcesRepo) FindDrive(ctx context.Context, driveID string) (*domain.VirtualDrive, error) {
	row := r.db.QueryRowContext(ctx, `
SELECT drive_id, library_id, slot, iqn, mount_state, mounted_cartridge_id, created_at, updated_at
FROM virtual_drives WHERE drive_id = ?`, strings.TrimSpace(driveID))
	return scanDrive(row)
}

func (r *CoreResourcesRepo) FindCartridge(ctx context.Context, cartridgeID string) (*domain.VirtualCartridge, error) {
	row := r.db.QueryRowContext(ctx, `
SELECT cartridge_id, pool_id, library_id, barcode, capacity_bytes, used_bytes,
       lifecycle_state, retention_state, assigned_slot_address, created_at, updated_at
FROM virtual_cartridges WHERE cartridge_id = ?`, strings.TrimSpace(cartridgeID))
	return scanCartridge(row)
}

func (r *CoreResourcesRepo) ListLibraries(ctx context.Context) []*domain.VirtualLibrary {
	rows, err := r.db.QueryContext(ctx, `
SELECT library_id, name, status, vendor, library_type, drive_type, drive_count,
       drive_start_address, slot_count, slot_start_address, ie_port_count,
       ie_start_address, iqn, compression_enabled, dedup_enabled, created_at, updated_at
FROM virtual_libraries ORDER BY library_id`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := make([]*domain.VirtualLibrary, 0)
	for rows.Next() {
		library, err := scanLibrary(rows)
		if err == nil {
			out = append(out, library)
		}
	}
	return out
}

func (r *CoreResourcesRepo) ListDrives(ctx context.Context) []*domain.VirtualDrive {
	rows, err := r.db.QueryContext(ctx, `
SELECT drive_id, library_id, slot, iqn, mount_state, mounted_cartridge_id, created_at, updated_at
FROM virtual_drives ORDER BY drive_id`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := make([]*domain.VirtualDrive, 0)
	for rows.Next() {
		drive, err := scanDrive(rows)
		if err == nil {
			out = append(out, drive)
		}
	}
	return out
}

func (r *CoreResourcesRepo) ListCartridges(ctx context.Context) []*domain.VirtualCartridge {
	rows, err := r.db.QueryContext(ctx, `
SELECT cartridge_id, pool_id, library_id, barcode, capacity_bytes, used_bytes,
       lifecycle_state, retention_state, assigned_slot_address, created_at, updated_at
FROM virtual_cartridges ORDER BY cartridge_id`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := make([]*domain.VirtualCartridge, 0)
	for rows.Next() {
		cartridge, err := scanCartridge(rows)
		if err == nil {
			out = append(out, cartridge)
		}
	}
	return out
}

type scanner interface {
	Scan(dest ...any) error
}

func scanLibrary(row scanner) (*domain.VirtualLibrary, error) {
	var library domain.VirtualLibrary
	var status, createdAt, updatedAt string
	var compressionEnabled, dedupEnabled int
	err := row.Scan(
		&library.LibraryID,
		&library.Name,
		&status,
		&library.Vendor,
		&library.LibraryType,
		&library.DriveType,
		&library.DriveCount,
		&library.DriveStartAddress,
		&library.SlotCount,
		&library.SlotStartAddress,
		&library.IEPortCount,
		&library.IEStartAddress,
		&library.IQN,
		&compressionEnabled,
		&dedupEnabled,
		&createdAt,
		&updatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	library.Status = domain.LibraryStatus(status)
	library.CompressionEnabled = compressionEnabled != 0
	library.DedupEnabled = dedupEnabled != 0
	library.CreatedAt = parseTime(createdAt)
	library.UpdatedAt = parseTime(updatedAt)
	return &library, nil
}

func scanDrive(row scanner) (*domain.VirtualDrive, error) {
	var drive domain.VirtualDrive
	var mountState, createdAt, updatedAt string
	err := row.Scan(
		&drive.DriveID,
		&drive.LibraryID,
		&drive.Slot,
		&drive.IQN,
		&mountState,
		&drive.MountedCartridgeID,
		&createdAt,
		&updatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	drive.MountState = domain.MountState(mountState)
	drive.CreatedAt = parseTime(createdAt)
	drive.UpdatedAt = parseTime(updatedAt)
	return &drive, nil
}

func scanCartridge(row scanner) (*domain.VirtualCartridge, error) {
	var cartridge domain.VirtualCartridge
	var lifecycleState, retentionState, createdAt, updatedAt string
	var assignedSlot sql.NullInt64
	err := row.Scan(
		&cartridge.CartridgeID,
		&cartridge.PoolID,
		&cartridge.LibraryID,
		&cartridge.Barcode,
		&cartridge.CapacityBytes,
		&cartridge.UsedBytes,
		&lifecycleState,
		&retentionState,
		&assignedSlot,
		&createdAt,
		&updatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	cartridge.LifecycleState = domain.CartridgeLifecycleState(lifecycleState)
	cartridge.RetentionState = domain.RetentionState(retentionState)
	if assignedSlot.Valid {
		value := int(assignedSlot.Int64)
		cartridge.AssignedSlotAddress = &value
	}
	cartridge.CreatedAt = parseTime(createdAt)
	cartridge.UpdatedAt = parseTime(updatedAt)
	return &cartridge, nil
}

func nullableInt(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}

func deleteByID(ctx context.Context, db *sql.DB, stmt, id string) error {
	result, err := db.ExecContext(ctx, stmt, strings.TrimSpace(id))
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func normalizeBarcodeKey(raw string) string {
	return strings.ToUpper(strings.TrimSpace(raw))
}

func (r *CoreResourcesRepo) isBarcodeDestroyed(ctx context.Context, barcodeKey string) (bool, error) {
	if strings.TrimSpace(barcodeKey) == "" {
		return false, nil
	}
	var existing string
	err := r.db.QueryRowContext(ctx, `SELECT barcode_key FROM destroyed_cartridge_barcodes WHERE barcode_key = ?`, barcodeKey).Scan(&existing)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return false, err
}

func isSQLiteConstraint(err error) bool {
	var sqliteErr *sqlitedriver.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	switch sqliteErr.Code() {
	case sqlite3.SQLITE_CONSTRAINT,
		sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY,
		sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY,
		sqlite3.SQLITE_CONSTRAINT_UNIQUE:
		return true
	default:
		return false
	}
}
