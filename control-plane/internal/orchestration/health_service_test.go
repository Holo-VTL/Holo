package orchestration

import (
	"errors"
	"net"
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
	runDir := shortTempDir(t)
	t.Setenv("HOLO_RUN_DIR", runDir)
	listener, err := net.Listen("unix", filepath.Join(runDir, "cdb.sock"))
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			t.Skipf("unix sockets are not permitted in this sandbox: %v", err)
		}
		t.Fatalf("listen on cdb socket: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	service := NewHealthServiceWithConfig(nil, nil, nil, nil, "", "")
	component := service.dataPlaneComponent()
	if component.Status != "ok" {
		t.Fatalf("expected data plane ok for configured run dir, got %+v", component)
	}
}

func TestDataPlaneComponentRejectsStaleSocketPath(t *testing.T) {
	runDir := shortTempDir(t)
	t.Setenv("HOLO_RUN_DIR", runDir)
	if err := os.WriteFile(filepath.Join(runDir, "cdb.sock"), []byte{}, 0o600); err != nil {
		t.Fatalf("write stale socket placeholder: %v", err)
	}

	service := NewHealthServiceWithConfig(nil, nil, nil, nil, "", "")
	component := service.dataPlaneComponent()
	if component.Status != "down" {
		t.Fatalf("expected stale data plane socket to be down, got %+v", component)
	}
}

func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "holo-health-")
	if err != nil {
		t.Fatalf("create short temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
