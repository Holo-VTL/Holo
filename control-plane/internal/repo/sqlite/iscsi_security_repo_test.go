package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
)

func TestISCSISecurityRepoPersistsBindingsAndOfflineTargets(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "security.db")
	repo := openTestDB(t, path)
	seedSecurityLibrary(t, repo)
	security := NewISCSISecurityRepo(repo.db)

	library := domain.ISCSISecurityBinding{Scope: domain.SecurityScopeLibrary, OwnerID: "lib-a", Generation: 1,
		Authentication: &domain.ISCSIAuthenticationPolicy{Mode: domain.ISCSIAuthCHAP, CredentialID: "cred-a", Initiators: []string{"iqn.1991-05.com.microsoft:backup-a"}}}
	if err := security.SaveBinding(ctx, library); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("expected an unprovisioned credential reference to be rejected, got %v", err)
	}
	if err := security.CreateCredential(ctx, testCredential()); err != nil {
		t.Fatalf("create credential: %v", err)
	}
	if err := security.SaveBinding(ctx, library); err != nil {
		t.Fatalf("save binding after references exist: %v", err)
	}
	drive := domain.ISCSISecurityBinding{Scope: domain.SecurityScopeDrive, OwnerID: "drive-a", LibraryID: "lib-a", Generation: 1}
	if err := security.SaveBinding(ctx, drive); err != nil {
		t.Fatalf("save drive binding: %v", err)
	}
	targetIQN := "iqn.2026-04.ai.holo:drive-a"
	target := domain.ISCSISecurityBinding{Scope: domain.SecurityScopeTarget, OwnerID: targetIQN, TargetIQN: targetIQN, LibraryID: "lib-a", DriveID: "drive-a", DeviceRole: "drive", Generation: 1,
		Authentication: &domain.ISCSIAuthenticationPolicy{Mode: domain.ISCSIAuthNone, RestrictInitiators: true}}
	if err := security.RegisterTarget(ctx, targetIQN, "lib-a", "drive-a", "drive"); err != nil {
		t.Fatalf("register target: %v", err)
	}
	target.Generation = 2
	if err := security.SaveBinding(ctx, target); err != nil {
		t.Fatalf("save target binding: %v", err)
	}
	if err := security.SetTargetOffline(ctx, targetIQN, true); err != nil {
		t.Fatalf("set offline intent: %v", err)
	}
	if err := repo.db.Close(); err != nil {
		t.Fatalf("close database: %v", err)
	}

	reopened := openTestDB(t, path)
	gotLibrary, err := NewISCSISecurityRepo(reopened.db).FindBinding(ctx, domain.SecurityScopeLibrary, "lib-a")
	if err != nil || gotLibrary.Authentication.CredentialID != "cred-a" {
		t.Fatalf("find library binding after reopen: binding=%+v err=%v", gotLibrary, err)
	}
	gotTarget, err := NewISCSISecurityRepo(reopened.db).FindTarget(ctx, targetIQN)
	if err != nil || !gotTarget.AdministrativeOffline || gotTarget.LibraryID != "lib-a" || gotTarget.DriveID != "drive-a" || gotTarget.Authentication == nil || !gotTarget.Authentication.RestrictInitiators {
		t.Fatalf("find target after reopen: target=%+v err=%v", gotTarget, err)
	}
}

func TestISCSISecurityRepoEnforcesVersionsAndOwnership(t *testing.T) {
	ctx := context.Background()
	repo := openTestDB(t, filepath.Join(t.TempDir(), "security.db"))
	seedSecurityLibrary(t, repo)
	security := NewISCSISecurityRepo(repo.db)
	binding := domain.ISCSISecurityBinding{Scope: domain.SecurityScopeLibrary, OwnerID: "lib-a", Generation: 1}
	if err := security.SaveBinding(ctx, binding); err != nil {
		t.Fatalf("create binding: %v", err)
	}
	stale := binding
	stale.Generation = 3
	if err := security.SaveBinding(ctx, stale); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("expected generation conflict, got %v", err)
	}
	updated := binding
	updated.Generation = 2
	updated.Authentication = &domain.ISCSIAuthenticationPolicy{Mode: domain.ISCSIAuthNone, Initiators: []string{"iqn.1991-05.com.microsoft:allowed"}}
	if err := security.SaveBinding(ctx, updated); err != nil {
		t.Fatalf("save next generation: %v", err)
	}
	if err := repo.DeleteLibrary(ctx, "lib-a"); err != nil {
		t.Fatalf("delete owning library cascades its binding: %v", err)
	}
	if _, err := security.FindBinding(ctx, domain.SecurityScopeLibrary, "lib-a"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expected deleted owner binding to disappear, got %v", err)
	}
}

func TestISCSISecurityRepoBoundsRedactedSnapshotHistoryAndProtectsReferences(t *testing.T) {
	ctx := context.Background()
	repo := openTestDB(t, filepath.Join(t.TempDir(), "security.db"))
	seedSecurityLibrary(t, repo)
	security := NewISCSISecurityRepo(repo.db)
	if err := security.CreateCredential(ctx, testCredential()); err != nil {
		t.Fatalf("create credential: %v", err)
	}
	for version := int64(1); version <= 22; version++ {
		snapshot := domain.ISCSISecuritySnapshot{
			SnapshotID: "lib-a-snapshot-" + string(rune('a'+version)), Scope: "library", OwnerID: "lib-a", Version: version,
			CredentialRefs: []string{"cred-a"}, Payload: []byte(`{"auth":{"credentialId":"cred-a"}}`),
			CreatedBy: "tester", CreatedAt: time.Now().UTC(),
		}
		if err := security.AppendSnapshot(ctx, snapshot); err != nil {
			t.Fatalf("append snapshot %d: %v", version, err)
		}
	}
	history, err := security.ListSnapshots(ctx, "library", "lib-a")
	if err != nil || len(history) != 20 || history[0].Version != 3 || history[19].Version != 22 {
		t.Fatalf("history should retain the newest 20 revisions: len=%d first=%+v last=%+v err=%v", len(history), history[0], history[len(history)-1], err)
	}
	if err := security.DeleteCredential(ctx, "cred-a"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("credential in a retained snapshot must not be deleted, got %v", err)
	}
}

func seedSecurityLibrary(t *testing.T, repo *CoreResourcesRepo) {
	t.Helper()
	library, err := domain.NewVirtualLibrary("lib-a", "Library A")
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveLibrary(context.Background(), library); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateDrive(context.Background(), mustSecurityDrive(t)); err != nil {
		t.Fatal(err)
	}
}

func mustSecurityDrive(t *testing.T) *domain.VirtualDrive {
	t.Helper()
	drive, err := domain.NewVirtualDrive("drive-a", "lib-a", 1)
	if err != nil {
		t.Fatal(err)
	}
	return drive
}

func testCredential() domain.ISCSICredential {
	return domain.ISCSICredential{CredentialID: "cred-a", Label: "backup host", Username: "backup", EncryptedSecret: make([]byte, 64), Version: 1, CreatedAt: time.Now().UTC()}
}
