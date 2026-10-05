package storageutil

import "strings"

// These projections mirror the names used by the storage layout and media
// state files. Repositories use them to reject two logical IDs that would
// otherwise address the same on-disk object.
func PoolRootProjection(poolID string) string {
	return SanitizeLayoutID(poolID)
}

func LibraryDirectoryProjection(libraryID string) string {
	return SanitizeLayoutID(libraryID)
}

func DriveDirectoryProjection(driveID string) string {
	return SanitizeLayoutID(driveID)
}

func CartridgeMetadataProjection(cartridgeID string) string {
	return SanitizeLayoutID(cartridgeID)
}

func CartridgeLayoutProjection(libraryID, cartridgeID string) string {
	return strings.Join([]string{LibraryDirectoryProjection(libraryID), CartridgeMetadataProjection(cartridgeID)}, "/")
}

func MediaStatePathProjection(libraryID, driveID string) string {
	return sanitizeMediaStateID(MediaStateKey(libraryID, driveID))
}
