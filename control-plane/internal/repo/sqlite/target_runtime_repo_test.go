package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
)

func TestTargetRuntimeRepoPersistsPublicationsAcrossReopen(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "metadata.db")
	coreRepo := openTestDB(t, dbPath)
	seedPublicationDependencies(t, ctx, coreRepo)

	repo := NewTargetRuntimeRepo(coreRepo.db)
	pub, err := domain.NewTargetPublication("pub-a", "pool-a", "lib-a", "drive-a", "cart-a", "iqn.2026-04.cloud.backupnext.holo:drive-a")
	if err != nil {
		t.Fatalf("new publication: %v", err)
	}
	pub.DeviceRole = "changer"
	pub.DeviceProfile = "scalar-i3"
	pub.DriveProfile = "lto8"
	pub.CompressionEnabled = false
	pub.DedupEnabled = false
	if err := pub.MarkReady("10.0.0.10:3260"); err != nil {
		t.Fatalf("mark ready: %v", err)
	}
	if err := repo.SavePublication(ctx, pub); err != nil {
		t.Fatalf("save publication: %v", err)
	}

	reopenedCore := openTestDB(t, dbPath)
	reopened := NewTargetRuntimeRepo(reopenedCore.db)
	got, err := reopened.FindPublication(ctx, "pub-a")
	if err != nil {
		t.Fatalf("find reopened publication: %v", err)
	}
	if got.TargetIQN != pub.TargetIQN || got.State != domain.PublicationReady || got.Portal != "10.0.0.10:3260" {
		t.Fatalf("unexpected reopened publication: %+v", got)
	}
	if got.DeviceRole != "changer" || got.DeviceProfile != "scalar-i3" || got.DriveProfile != "lto8" {
		t.Fatalf("unexpected reopened device identity: %+v", got)
	}
	if got.CompressionEnabled || got.DedupEnabled {
		t.Fatalf("unexpected reopened tape policy: %+v", got)
	}
}

func TestTargetRuntimeRepoRejectsActiveDuplicateIQN(t *testing.T) {
	ctx := context.Background()
	coreRepo := openTestDB(t, filepath.Join(t.TempDir(), "metadata.db"))
	seedPublicationDependencies(t, ctx, coreRepo)
	repo := NewTargetRuntimeRepo(coreRepo.db)

	first, _ := domain.NewTargetPublication("pub-a", "pool-a", "lib-a", "drive-a", "cart-a", "iqn.2026-04.cloud.backupnext.holo:drive-a")
	if err := first.MarkReady("10.0.0.10:3260"); err != nil {
		t.Fatalf("mark first ready: %v", err)
	}
	if err := repo.SavePublicationIfIQNAvailable(ctx, first); err != nil {
		t.Fatalf("save first: %v", err)
	}
	second, _ := domain.NewTargetPublication("pub-b", "pool-a", "lib-a", "drive-a", "cart-a", first.TargetIQN)
	if err := repo.SavePublicationIfIQNAvailable(ctx, second); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("expected conflict, got %v", err)
	}
	if err := first.Disable(); err != nil {
		t.Fatalf("disable first: %v", err)
	}
	if err := repo.SavePublication(ctx, first); err != nil {
		t.Fatalf("save disabled first: %v", err)
	}
	if err := repo.SavePublicationIfIQNAvailable(ctx, second); err != nil {
		t.Fatalf("expected disabled publication to release IQN, got %v", err)
	}
}

func TestTargetRuntimeRepoReplacesStaleCreatingPublication(t *testing.T) {
	ctx := context.Background()
	coreRepo := openTestDB(t, filepath.Join(t.TempDir(), "metadata.db"))
	seedPublicationDependencies(t, ctx, coreRepo)
	repo := NewTargetRuntimeRepo(coreRepo.db)

	stale, _ := domain.NewTargetPublication("pub-stale", "pool-a", "lib-a", "drive-a", "cart-a", "iqn.2026-04.cloud.backupnext.holo:drive-a")
	if err := repo.SavePublication(ctx, stale); err != nil {
		t.Fatalf("save stale creating publication: %v", err)
	}
	retry, _ := domain.NewTargetPublication("pub-retry", "pool-a", "lib-a", "drive-a", "cart-a", stale.TargetIQN)
	if err := repo.SavePublicationIfIQNAvailable(ctx, retry); err != nil {
		t.Fatalf("expected retry to replace stale creating publication, got %v", err)
	}

	reloadedStale, err := repo.FindPublication(ctx, "pub-stale")
	if err != nil {
		t.Fatalf("find stale publication: %v", err)
	}
	if reloadedStale.State != domain.PublicationFailed {
		t.Fatalf("expected stale publication to be failed, got %+v", reloadedStale)
	}
	if _, ok := repo.FindPublicationByIQN(ctx, stale.TargetIQN); !ok {
		t.Fatalf("expected retry publication to own active IQN")
	}
}

func seedPublicationDependencies(t *testing.T, ctx context.Context, repo *CoreResourcesRepo) {
	t.Helper()
	pool, err := domain.NewStoragePoolRuntime("pool-a", "Pool A", 80)
	if err != nil {
		t.Fatalf("new pool: %v", err)
	}
	if err := NewStoragePoolRepo(repo.db).SavePool(ctx, pool); err != nil {
		t.Fatalf("save pool: %v", err)
	}
	lib, err := domain.NewVirtualLibrary("lib-a", "Library A")
	if err != nil {
		t.Fatalf("new library: %v", err)
	}
	if err := repo.SaveLibrary(ctx, lib); err != nil {
		t.Fatalf("save library: %v", err)
	}
	drive, err := domain.NewVirtualDrive("drive-a", "lib-a", 1)
	if err != nil {
		t.Fatalf("new drive: %v", err)
	}
	if err := repo.SaveDrive(ctx, drive); err != nil {
		t.Fatalf("save drive: %v", err)
	}
	cartridge := domain.NewVirtualCartridge("cart-a", "pool-a", "lib-a", "VTA000L06", 1024)
	if err := repo.SaveCartridge(ctx, cartridge); err != nil {
		t.Fatalf("save cartridge: %v", err)
	}
}
