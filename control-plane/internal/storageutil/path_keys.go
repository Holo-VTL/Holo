package storageutil

import (
	"os"
	"regexp"
	"strings"
)

var devicePathRE = regexp.MustCompile(`^/dev/[A-Za-z0-9._-]+$`)

func NormalizeDevicePath(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, "/dev/") {
		return raw
	}
	if strings.HasPrefix(raw, "dev/") {
		return "/" + raw
	}
	return "/dev/" + raw
}

func IsSafeDevicePath(path string) bool {
	return devicePathRE.MatchString(strings.TrimSpace(path))
}

func MediaStateKey(libraryID, driveID string) string {
	libraryID = strings.TrimSpace(libraryID)
	driveID = strings.TrimSpace(driveID)
	if libraryID == "" {
		libraryID = "unknown"
	}
	if driveID == "" {
		driveID = "unknown"
	}
	return libraryID + "__" + driveID
}

func StrictStorageFlowEnabled() bool {
	raw := strings.TrimSpace(strings.ToLower(os.Getenv("HOLO_STRICT_STORAGE_FLOW")))
	if raw == "" {
		return true
	}
	switch raw {
	case "0", "false", "off", "no":
		return false
	default:
		return true
	}
}
