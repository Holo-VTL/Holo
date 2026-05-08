package domain

import (
	"strings"
	"time"
)

type LibraryStatus string

const (
	LibraryReady       LibraryStatus = "ready"
	LibraryMaintenance LibraryStatus = "maintenance"
	LibraryError       LibraryStatus = "error"
)

type VirtualLibrary struct {
	Timestamped
	LibraryID          string        `json:"libraryId"`
	Name               string        `json:"name"`
	Status             LibraryStatus `json:"status"`
	Vendor             string        `json:"vendor,omitempty"`
	LibraryType        string        `json:"libraryType,omitempty"`
	DriveType          string        `json:"driveType,omitempty"`
	DriveCount         int           `json:"driveCount,omitempty"`
	DriveStartAddress  int           `json:"driveStartAddress,omitempty"`
	SlotCount          int           `json:"slotCount,omitempty"`
	SlotStartAddress   int           `json:"slotStartAddress,omitempty"`
	IEPortCount        int           `json:"iePortCount,omitempty"`
	IEStartAddress     int           `json:"ieStartAddress,omitempty"`
	IQN                string        `json:"iqn,omitempty"`
	CompressionEnabled bool          `json:"compressionEnabled"`
	DedupEnabled       bool          `json:"dedupEnabled"`
}

func NewVirtualLibrary(id, name string) (*VirtualLibrary, error) {
	if ValidateManagementID(id) != nil || ValidateManagementLabel(name, true) != nil {
		return nil, ErrInvalidInput
	}
	now := time.Now().UTC()
	return &VirtualLibrary{
		Timestamped: Timestamped{CreatedAt: now, UpdatedAt: now},
		LibraryID:   id,
		Name:        name,
		Status:      LibraryReady,
		IQN:         defaultLibraryIQN(id),
	}, nil
}

func defaultLibraryIQN(libraryID string) string {
	token := sanitizeIQNToken(libraryID, "library")
	return "iqn.2026-04.cloud.backupnext.holo:library-" + token
}

func sanitizeIQNToken(raw, fallback string) string {
	normalized := strings.ToLower(strings.TrimSpace(raw))
	var b strings.Builder
	for _, ch := range normalized {
		if (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '-' || ch == '.' {
			b.WriteRune(ch)
		} else {
			b.WriteByte('-')
		}
	}
	token := strings.Trim(b.String(), "-.")
	if token == "" {
		return fallback
	}
	return token
}
