package sqlite

import (
	"context"
	"path/filepath"
	"testing"
)

func TestOpenCreatesSchemaAndRecordsMigration(t *testing.T) {
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatalf("open sqlite db: %v", err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = 1`).Scan(&count); err != nil {
		t.Fatalf("query schema migrations: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected migration version 1 recorded, got %d", count)
	}
}

func TestOpenFailsForUnusablePath(t *testing.T) {
	_, err := Open(context.Background(), filepath.Join(t.TempDir(), "missing", "metadata.db"))
	if err == nil {
		t.Fatal("expected open to fail for missing parent directory")
	}
}
