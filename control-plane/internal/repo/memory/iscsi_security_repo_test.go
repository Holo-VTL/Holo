package memory

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
)

func TestISCSISecurityRepoScopesVersionsAndStableTarget(t *testing.T) {
	ctx := context.Background()
	repo := NewISCSISecurityRepo()
	library := domain.ISCSISecurityBinding{Scope: domain.SecurityScopeLibrary, OwnerID: "lib-a", Generation: 1}
	if err := repo.SaveBinding(ctx, library); err != nil {
		t.Fatal(err)
	}
	updated := library
	updated.Generation = 3
	if err := repo.SaveBinding(ctx, updated); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("expected generation conflict, got %v", err)
	}
	targetIQN := "iqn.2026-04.ai.holo:drive-a"
	if err := repo.RegisterTarget(ctx, targetIQN, "lib-a", "drive-a", "drive"); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetTargetOffline(ctx, targetIQN, true); err != nil {
		t.Fatal(err)
	}
	target, err := repo.FindTarget(ctx, targetIQN)
	if err != nil || !target.AdministrativeOffline || target.DriveID != "drive-a" {
		t.Fatalf("unexpected stable target: %+v, %v", target, err)
	}
	if err := repo.RegisterTarget(ctx, targetIQN, "lib-b", "drive-b", "drive"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("target identity must not transfer to another owner, got %v", err)
	}
}

func TestISCSISecurityRepoPreservesExplicitDenyAllACL(t *testing.T) {
	ctx := context.Background()
	repository := NewISCSISecurityRepo()
	targetIQN := "iqn.2026-04.ai.holo:drive-a"
	if err := repository.RegisterTarget(ctx, targetIQN, "lib-a", "drive-a", "drive"); err != nil {
		t.Fatal(err)
	}
	binding, err := repository.FindTarget(ctx, targetIQN)
	if err != nil {
		t.Fatal(err)
	}
	binding.Generation++
	binding.Authentication = &domain.ISCSIAuthenticationPolicy{Mode: domain.ISCSIAuthNone, RestrictInitiators: true}
	if err := repository.SaveBinding(ctx, binding); err != nil {
		t.Fatal(err)
	}
	got, err := repository.FindTarget(ctx, targetIQN)
	if err != nil || got.Authentication == nil || !got.Authentication.RestrictInitiators || len(got.Authentication.Initiators) != 0 {
		t.Fatalf("deny-all ACL state was lost: %+v, %v", got.Authentication, err)
	}
}

func TestISCSISecurityRepoRetainsHistoryAndPreventsDeletingReferencedMaterials(t *testing.T) {
	ctx := context.Background()
	repo := NewISCSISecurityRepo()
	if err := repo.CreateCredential(ctx, memorySecurityCredential()); err != nil {
		t.Fatal(err)
	}
	for version := int64(1); version <= 22; version++ {
		snapshot := domain.ISCSISecuritySnapshot{
			SnapshotID: fmt.Sprintf("snapshot-%02d", version), Scope: "library", OwnerID: "lib-a", Version: version,
			CredentialRefs: []string{"cred-a"}, Payload: []byte(`{"auth":{"credentialId":"cred-a"}}`), CreatedBy: "tester", CreatedAt: time.Now().UTC(),
		}
		if err := repo.AppendSnapshot(ctx, snapshot); err != nil {
			t.Fatal(err)
		}
	}
	history, err := repo.ListSnapshots(ctx, "library", "lib-a")
	if err != nil || len(history) != 20 || history[0].Version != 3 || history[19].Version != 22 {
		t.Fatalf("unexpected bounded history: count=%d err=%v", len(history), err)
	}
	if err := repo.DeleteCredential(ctx, "cred-a"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("referenced credential deletion should conflict, got %v", err)
	}
}

func memorySecurityCredential() domain.ISCSICredential {
	return domain.ISCSICredential{CredentialID: "cred-a", Label: "backup host", Username: "backup", EncryptedSecret: make([]byte, 64), Version: 1, CreatedAt: time.Now().UTC()}
}
