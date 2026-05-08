package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
)

func TestPolicyReposPersistAcrossReopen(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "metadata.db")
	db, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open sqlite db: %v", err)
	}
	accessRepo := NewAccessPolicyRepo(db)
	retentionRepo := NewRetentionPolicyRepo(db)
	access := domain.TargetAccessPolicy{
		PolicyID:      "policy-a",
		Scope:         domain.ScopeLibrary,
		Subject:       "iqn.2026-04.ai.holo:init-a",
		Permission:    domain.PermissionAllow,
		EffectiveFrom: time.Now().UTC(),
		EffectiveTo:   time.Now().UTC().Add(time.Hour),
	}
	if err := accessRepo.Save(ctx, access); err != nil {
		t.Fatalf("save access policy: %v", err)
	}
	retention := domain.NewRetentionPolicy("ret-a", "VTA000L06", domain.RetentionModeWORM, time.Now().UTC().Add(time.Hour), "tester")
	if err := retentionRepo.Save(ctx, retention); err != nil {
		t.Fatalf("save retention policy: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	reopened, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("reopen sqlite db: %v", err)
	}
	defer reopened.Close()
	accessList, err := NewAccessPolicyRepo(reopened).List(ctx)
	if err != nil {
		t.Fatalf("list access policies: %v", err)
	}
	if len(accessList) != 1 || accessList[0].PolicyID != "policy-a" || accessList[0].Permission != domain.PermissionAllow {
		t.Fatalf("unexpected access policies after reopen: %+v", accessList)
	}
	gotRetention, err := NewRetentionPolicyRepo(reopened).Find(ctx, "ret-a")
	if err != nil {
		t.Fatalf("find retention policy: %v", err)
	}
	if gotRetention.CartridgeID != "VTA000L06" || gotRetention.Mode != domain.RetentionModeWORM || gotRetention.CreatedBy != "tester" {
		t.Fatalf("unexpected retention policy after reopen: %+v", gotRetention)
	}
}
