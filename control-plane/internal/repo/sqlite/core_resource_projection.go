package sqlite

import (
	"context"
	"database/sql"
	"strings"

	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
	"github.com/Holo-VTL/Holo/control-plane/internal/storageutil"
)

func libraryProjectionConflict(ctx context.Context, tx *sql.Tx, candidate *domain.VirtualLibrary, excludeID string) (bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT library_id, iqn FROM virtual_libraries`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	component := storageutil.LibraryDirectoryProjection(candidate.LibraryID)
	iqn := resourceIQNProjection(candidate.IQN)
	for rows.Next() {
		var id, existingIQN string
		if err := rows.Scan(&id, &existingIQN); err != nil {
			return false, err
		}
		if id == excludeID {
			continue
		}
		if storageutil.LibraryDirectoryProjection(id) == component || iqn != "" && resourceIQNProjection(existingIQN) == iqn {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	rows.Close()
	return iqnCollisionWithDrives(ctx, tx, iqn)
}

func driveProjectionConflict(ctx context.Context, tx *sql.Tx, candidate *domain.VirtualDrive, excludeID string) (bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT drive_id, library_id, iqn FROM virtual_drives`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	component := storageutil.DriveDirectoryProjection(candidate.DriveID)
	statePath := storageutil.MediaStatePathProjection(candidate.LibraryID, candidate.DriveID)
	iqn := resourceIQNProjection(candidate.IQN)
	for rows.Next() {
		var id, libraryID, existingIQN string
		if err := rows.Scan(&id, &libraryID, &existingIQN); err != nil {
			return false, err
		}
		if id == excludeID {
			continue
		}
		if storageutil.DriveDirectoryProjection(id) == component ||
			storageutil.MediaStatePathProjection(libraryID, id) == statePath ||
			iqn != "" && resourceIQNProjection(existingIQN) == iqn {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	rows.Close()
	return iqnCollisionWithLibraries(ctx, tx, iqn)
}

func cartridgeProjectionConflict(ctx context.Context, tx *sql.Tx, candidate *domain.VirtualCartridge, excludeID string) (bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT cartridge_id, library_id FROM virtual_cartridges`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	metadata := storageutil.CartridgeMetadataProjection(candidate.CartridgeID)
	layout := storageutil.CartridgeLayoutProjection(candidate.LibraryID, candidate.CartridgeID)
	for rows.Next() {
		var id, libraryID string
		if err := rows.Scan(&id, &libraryID); err != nil {
			return false, err
		}
		if id == excludeID {
			continue
		}
		if storageutil.CartridgeMetadataProjection(id) == metadata ||
			storageutil.CartridgeLayoutProjection(libraryID, id) == layout {
			return true, nil
		}
	}
	return false, rows.Err()
}

func iqnCollisionWithDrives(ctx context.Context, tx *sql.Tx, iqn string) (bool, error) {
	if iqn == "" {
		return false, nil
	}
	var found string
	err := tx.QueryRowContext(ctx, `SELECT drive_id FROM virtual_drives WHERE lower(trim(iqn))=? LIMIT 1`, iqn).Scan(&found)
	if err == nil {
		return true, nil
	}
	if err != sql.ErrNoRows {
		return false, err
	}
	return false, nil
}

func iqnCollisionWithLibraries(ctx context.Context, tx *sql.Tx, iqn string) (bool, error) {
	if iqn == "" {
		return false, nil
	}
	var found string
	err := tx.QueryRowContext(ctx, `SELECT library_id FROM virtual_libraries WHERE lower(trim(iqn))=? LIMIT 1`, iqn).Scan(&found)
	if err == nil {
		return true, nil
	}
	if err != sql.ErrNoRows {
		return false, err
	}
	return false, nil
}

func resourceIQNProjection(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func barcodeDestroyed(ctx context.Context, tx *sql.Tx, barcodeKey string) (bool, error) {
	if strings.TrimSpace(barcodeKey) == "" {
		return false, nil
	}
	var existing string
	err := tx.QueryRowContext(ctx, `SELECT barcode_key FROM destroyed_cartridge_barcodes WHERE barcode_key = ?`, barcodeKey).Scan(&existing)
	if err == nil {
		return true, nil
	}
	if err == sql.ErrNoRows {
		return false, nil
	}
	return false, err
}
