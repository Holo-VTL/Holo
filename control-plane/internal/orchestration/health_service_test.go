package orchestration

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDatabaseComponentAcceptsSQLitePathDSN(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "metadata.db")
	if err := os.WriteFile(dbPath, []byte("sqlite placeholder"), 0o600); err != nil {
		t.Fatalf("write sqlite file: %v", err)
	}

	service := NewHealthServiceWithConfig(nil, nil, nil, nil, dbPath, "")

	component := service.databaseComponent()
	if component.Status != "ok" {
		t.Fatalf("expected sqlite path dsn to be ok, got %+v", component)
	}
}

func TestDatabaseComponentAcceptsSQLiteFileDSN(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "metadata.db")
	if err := os.WriteFile(dbPath, []byte("sqlite placeholder"), 0o600); err != nil {
		t.Fatalf("write sqlite file: %v", err)
	}

	service := NewHealthServiceWithConfig(nil, nil, nil, nil, "file://"+dbPath, "")

	component := service.databaseComponent()
	if component.Status != "ok" {
		t.Fatalf("expected sqlite file dsn to be ok, got %+v", component)
	}
}

func TestDatabaseComponentReportsMissingSQLitePathDown(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "missing.db")
	service := NewHealthServiceWithConfig(nil, nil, nil, nil, dbPath, "")

	component := service.databaseComponent()
	if component.Status != "down" {
		t.Fatalf("expected missing sqlite path to be down, got %+v", component)
	}
}

func TestDataPlaneComponentUsesConfiguredRunDir(t *testing.T) {
	runDir := t.TempDir()
	t.Setenv("HOLO_RUN_DIR", runDir)
	if err := os.WriteFile(filepath.Join(runDir, "cdb.sock"), []byte{}, 0o600); err != nil {
		t.Fatalf("write socket placeholder: %v", err)
	}

	service := NewHealthServiceWithConfig(nil, nil, nil, nil, "", "")
	component := service.dataPlaneComponent()
	if component.Status != "ok" {
		t.Fatalf("expected data plane ok for configured run dir, got %+v", component)
	}
}
