package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	sqlitedriver "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
	"github.com/Holo-VTL/Holo/control-plane/internal/repo"
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

func (r *LocalMountRepo) SaveLibraryMapping(ctx context.Context, mapping domain.LocalLoopbackLibraryMapping) error {
	if mapping.Validate() != nil {
		return domain.ErrInvalidInput
	}
	var existing domain.LocalLoopbackLibraryMapping
	err := r.db.QueryRowContext(ctx, `SELECT library_id, target_naa, nexus_naa, tpg_tag FROM local_loopback_libraries WHERE library_id = ?`, mapping.LibraryID).
		Scan(&existing.LibraryID, &existing.TargetNAA, &existing.NexusNAA, &existing.TPGTag)
	if err == nil {
		if existing != mapping {
			return domain.ErrConflict
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = r.db.ExecContext(ctx, `INSERT INTO local_loopback_libraries(library_id, target_naa, nexus_naa, tpg_tag) VALUES (?, ?, ?, ?)`, mapping.LibraryID, mapping.TargetNAA, mapping.NexusNAA, mapping.TPGTag)
	return mapLocalMountRepoError(err)
}

func (r *LocalMountRepo) ListLibraryMappings(ctx context.Context) ([]domain.LocalLoopbackLibraryMapping, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT library_id, target_naa, nexus_naa, tpg_tag FROM local_loopback_libraries ORDER BY library_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]domain.LocalLoopbackLibraryMapping, 0)
	for rows.Next() {
		var mapping domain.LocalLoopbackLibraryMapping
		if err := rows.Scan(&mapping.LibraryID, &mapping.TargetNAA, &mapping.NexusNAA, &mapping.TPGTag); err != nil {
			return nil, err
		}
		out = append(out, mapping)
	}
	return out, rows.Err()
}

func (r *LocalMountRepo) DeleteLibraryMapping(ctx context.Context, libraryID string) error {
	result, err := r.db.ExecContext(ctx, `DELETE FROM local_loopback_libraries WHERE library_id = ?`, libraryID)
	if err != nil {
		return mapLocalMountRepoError(err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func (r *LocalMountRepo) SaveDeviceMapping(ctx context.Context, mapping domain.LocalLoopbackDeviceMapping) error {
	if mapping.Validate() != nil {
		return domain.ErrInvalidInput
	}
	var existing domain.LocalLoopbackDeviceMapping
	err := r.db.QueryRowContext(ctx, `SELECT device_key, library_id, kind, COALESCE(drive_id, ''), lun_index, identity_ref, backend_ref, state FROM local_loopback_devices WHERE device_key = ?`, mapping.DeviceKey).
		Scan(&existing.DeviceKey, &existing.LibraryID, &existing.Kind, &existing.DriveID, &existing.LUNIndex, &existing.IdentityRef, &existing.BackendRef, &existing.State)
	if err == nil {
		if existing.LibraryID != mapping.LibraryID || existing.Kind != mapping.Kind || existing.DriveID != mapping.DriveID || existing.LUNIndex != mapping.LUNIndex || existing.IdentityRef != mapping.IdentityRef || existing.BackendRef != mapping.BackendRef || existing.State != mapping.State {
			return domain.ErrConflict
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var libraryID string
	if err := r.db.QueryRowContext(ctx, `SELECT library_id FROM local_loopback_libraries WHERE library_id = ?`, mapping.LibraryID).Scan(&libraryID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.ErrNotFound
		}
		return err
	}
	_, err = r.db.ExecContext(ctx, `INSERT INTO local_loopback_devices(device_key, library_id, kind, drive_id, lun_index, identity_ref, backend_ref, state) VALUES (?, ?, ?, NULLIF(?, ''), ?, ?, ?, ?)`, mapping.DeviceKey, mapping.LibraryID, mapping.Kind, mapping.DriveID, mapping.LUNIndex, mapping.IdentityRef, mapping.BackendRef, mapping.State)
	return mapLocalMountRepoError(err)
}

func (r *LocalMountRepo) ListDeviceMappings(ctx context.Context, libraryID string) ([]domain.LocalLoopbackDeviceMapping, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT device_key, library_id, kind, COALESCE(drive_id, ''), lun_index, identity_ref, backend_ref, state FROM local_loopback_devices WHERE library_id = ? ORDER BY device_key`, libraryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]domain.LocalLoopbackDeviceMapping, 0)
	for rows.Next() {
		var mapping domain.LocalLoopbackDeviceMapping
		if err := rows.Scan(&mapping.DeviceKey, &mapping.LibraryID, &mapping.Kind, &mapping.DriveID, &mapping.LUNIndex, &mapping.IdentityRef, &mapping.BackendRef, &mapping.State); err != nil {
			return nil, err
		}
		out = append(out, mapping)
	}
	return out, rows.Err()
}

func (r *LocalMountRepo) MarkDeviceCleanupPending(ctx context.Context, deviceKey string) error {
	return r.setDeviceState(ctx, deviceKey, domain.LocalMappingStateCleanupPending)
}

func (r *LocalMountRepo) MarkDeviceActive(ctx context.Context, deviceKey string) error {
	return r.setDeviceState(ctx, deviceKey, domain.LocalMappingStateActive)
}

func (r *LocalMountRepo) MarkDeviceInactive(ctx context.Context, deviceKey string) error {
	return r.setDeviceState(ctx, deviceKey, domain.LocalMappingStateInactive)
}

func (r *LocalMountRepo) setDeviceState(ctx context.Context, deviceKey string, state domain.LocalMappingState) error {
	result, err := r.db.ExecContext(ctx, `UPDATE local_loopback_devices SET state = ? WHERE device_key = ?`, state, deviceKey)
	if err != nil {
		return mapLocalMountRepoError(err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func (r *LocalMountRepo) DeleteDeviceMapping(ctx context.Context, deviceKey string) error {
	result, err := r.db.ExecContext(ctx, `DELETE FROM local_loopback_devices WHERE device_key = ?`, deviceKey)
	if err != nil {
		return mapLocalMountRepoError(err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func mapLocalMountRepoError(err error) error {
	if err == nil {
		return nil
	}
	var sqliteErr *sqlitedriver.Error
	if errors.As(err, &sqliteErr) && sqliteErr.Code()&0xff == sqlite3.SQLITE_CONSTRAINT {
		return domain.ErrConflict
	}
	return err
}

var _ repo.LocalMountRepository = (*LocalMountRepo)(nil)
