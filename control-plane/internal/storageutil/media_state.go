package storageutil

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func MediaStateDir() string {
	if raw := strings.TrimSpace(os.Getenv("HOLO_MEDIA_STATE_DIR")); raw != "" {
		return raw
	}
	return "/run/holo/media-state"
}

func MediaStatePath(libraryID, driveID string) (string, error) {
	libraryID = strings.TrimSpace(libraryID)
	driveID = strings.TrimSpace(driveID)
	if libraryID == "" || driveID == "" {
		return "", fmt.Errorf("library and drive IDs are required")
	}
	stateKey := MediaStateKey(libraryID, driveID)
	return filepath.Join(MediaStateDir(), sanitizeMediaStateID(stateKey)+".state"), nil
}

func ReadDriveMediaState(libraryID, driveID string) (string, error) {
	path, err := MediaStatePath(libraryID, driveID)
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return "", nil
	}
	if strings.HasPrefix(trimmed, "cartridge=") {
		value := strings.TrimSpace(strings.TrimPrefix(trimmed, "cartridge="))
		if strings.ContainsAny(value, "\r\n=") {
			return "", fmt.Errorf("invalid shared media state format")
		}
		return value, nil
	}
	if strings.ContainsAny(trimmed, "\r\n=") {
		return "", fmt.Errorf("invalid shared media state format")
	}
	return trimmed, nil
}

func sanitizeMediaStateID(raw string) string {
	raw = strings.TrimSpace(strings.ToLower(raw))
	if raw == "" {
		return "unknown"
	}
	var b strings.Builder
	for _, ch := range raw {
		if (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '-' || ch == '_' {
			b.WriteRune(ch)
		} else {
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "unknown"
	}
	return b.String()
}
