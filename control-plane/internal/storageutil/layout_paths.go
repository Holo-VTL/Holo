package storageutil

import (
	"os"
	"path/filepath"
	"strings"
)

func ResolveStorageRoot() string {
	if raw := strings.TrimSpace(os.Getenv("HOLO_STORAGE_ROOT")); raw != "" {
		return raw
	}

	preferred := "/var/lib/holo/storage"
	if canWriteDir(preferred) {
		return preferred
	}

	home, err := os.UserHomeDir()
	if err == nil && strings.TrimSpace(home) != "" {
		homeRoot := filepath.Join(home, ".local", "share", "holo", "storage")
		if canWriteDir(homeRoot) {
			return homeRoot
		}
	}

	return "/tmp/holo-storage"
}

func ResolvePoolStorageBaseDir() string {
	if raw := strings.TrimSpace(os.Getenv("HOLO_STORAGE_POOL_ROOT_BASE")); raw != "" {
		return raw
	}
	return "/var/lib/holo/storage-pools"
}

func PoolStorageRoot(poolID string) string {
	return filepath.Join(ResolvePoolStorageBaseDir(), SanitizeLayoutID(poolID))
}

func CanonicalCartridgeLayoutDir(storageRoot, libraryID, cartridgeID string) string {
	return filepath.Join(
		strings.TrimSpace(storageRoot),
		"cartridges",
		SanitizeLayoutID(libraryID),
		SanitizeLayoutID(cartridgeID),
	)
}

func LegacyCartridgeLayoutDirs(storageRoot, cartridgeID string) ([]string, error) {
	root := strings.TrimSpace(storageRoot)
	if root == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	needle := SanitizeLayoutID(cartridgeID)
	out := make([]string, 0)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if entry.Name() == "cartridges" {
			continue
		}
		candidate := filepath.Join(root, entry.Name(), needle)
		stat, statErr := os.Stat(candidate)
		if statErr != nil || !stat.IsDir() {
			continue
		}
		out = append(out, candidate)
	}
	return out, nil
}

func SanitizeLayoutID(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "unknown"
	}
	var b strings.Builder
	for _, ch := range raw {
		switch {
		case ch >= 'a' && ch <= 'z':
			b.WriteRune(ch)
		case ch >= 'A' && ch <= 'Z':
			b.WriteRune(ch + ('a' - 'A'))
		case ch >= '0' && ch <= '9':
			b.WriteRune(ch)
		case ch == '-' || ch == '_':
			b.WriteRune(ch)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "" {
		return "unknown"
	}
	return out
}

func canWriteDir(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		return false
	}
	probe := filepath.Join(path, ".write-probe")
	if err := os.WriteFile(probe, []byte("probe"), 0o600); err != nil {
		return false
	}
	_ = os.Remove(probe)
	return true
}
