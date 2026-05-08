package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
)

func openTestDB(t *testing.T, path string) *CoreResourcesRepo {
	t.Helper()
	db, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open sqlite db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewCoreResourcesRepo(db)
}

func saveCoreRepoTestPool(t *testing.T, ctx context.Context, repo *CoreResourcesRepo, poolID string) {
	t.Helper()
	pool, err := domain.NewStoragePoolRuntime(poolID, poolID, 80)
	if err != nil {
		t.Fatalf("new storage pool: %v", err)
	}
	if err := NewStoragePoolRepo(repo.db).SavePool(ctx, pool); err != nil {
		t.Fatalf("save storage pool: %v", err)
	}
}

func TestCoreResourcesRepoPersistsAcrossReopen(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "metadata.db")
	repo := openTestDB(t, dbPath)

	lib, err := domain.NewVirtualLibrary("lib-a", "Library A")
	if err != nil {
		t.Fatalf("new library: %v", err)
	}
	lib.DriveCount = 4
	lib.SlotCount = 20
	lib.CompressionEnabled = false
	lib.DedupEnabled = false
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
	saveCoreRepoTestPool(t, ctx, repo, "pool-a")
	cartridge := domain.NewVirtualCartridge("VTA000L06", "pool-a", "lib-a", "VTA000L06", 1024)
	if err := repo.SaveCartridge(ctx, cartridge); err != nil {
		t.Fatalf("save cartridge: %v", err)
	}

	reopened := openTestDB(t, dbPath)
	gotLib, err := reopened.FindLibrary(ctx, "lib-a")
	if err != nil {
		t.Fatalf("find reopened library: %v", err)
	}
	if gotLib.DriveCount != 4 || gotLib.SlotCount != 20 || gotLib.IQN == "" || gotLib.CompressionEnabled || gotLib.DedupEnabled {
		t.Fatalf("unexpected reopened library: %+v", gotLib)
	}
	gotDrive, err := reopened.FindDrive(ctx, "drive-a")
	if err != nil {
		t.Fatalf("find reopened drive: %v", err)
	}
	if gotDrive.LibraryID != "lib-a" || gotDrive.Slot != 1 || gotDrive.MountState != domain.MountEmpty {
		t.Fatalf("unexpected reopened drive: %+v", gotDrive)
	}
	gotCart, err := reopened.FindCartridge(ctx, "VTA000L06")
	if err != nil {
		t.Fatalf("find reopened cartridge: %v", err)
	}
	if gotCart.PoolID != "pool-a" || gotCart.LibraryID != "lib-a" || gotCart.CapacityBytes != 1024 {
		t.Fatalf("unexpected reopened cartridge: %+v", gotCart)
	}
}

func TestCoreResourcesRepoRejectsDuplicateBarcode(t *testing.T) {
	ctx := context.Background()
	repo := openTestDB(t, filepath.Join(t.TempDir(), "metadata.db"))
	lib, _ := domain.NewVirtualLibrary("lib-a", "Library A")
	if err := repo.SaveLibrary(ctx, lib); err != nil {
		t.Fatalf("save library: %v", err)
	}
	saveCoreRepoTestPool(t, ctx, repo, "pool-a")
	first := domain.NewVirtualCartridge("cart-a", "pool-a", "lib-a", "VTA000L06", 1024)
	second := domain.NewVirtualCartridge("cart-b", "pool-a", "lib-a", "vta000l06", 1024)
	if err := repo.SaveCartridge(ctx, first); err != nil {
		t.Fatalf("save first cartridge: %v", err)
	}
	if err := repo.SaveCartridge(ctx, second); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("expected duplicate barcode conflict, got %v", err)
	}
}

func TestCoreResourcesRepoRejectsDestroyedBarcodeReuse(t *testing.T) {
	ctx := context.Background()
	repo := openTestDB(t, filepath.Join(t.TempDir(), "metadata.db"))
	lib, _ := domain.NewVirtualLibrary("lib-a", "Library A")
	if err := repo.SaveLibrary(ctx, lib); err != nil {
		t.Fatalf("save library: %v", err)
	}
	saveCoreRepoTestPool(t, ctx, repo, "pool-a")

	if err := repo.RetireCartridgeBarcode(ctx, "VTA123L06", "cart-old", "tester"); err != nil {
		t.Fatalf("retire barcode: %v", err)
	}
	cartridge := domain.NewVirtualCartridge("cart-new", "pool-a", "lib-a", "vta123l06", 1024)
	if err := repo.SaveCartridge(ctx, cartridge); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("expected destroyed barcode conflict, got %v", err)
	}
}

func TestCoreResourcesRepoDestroyedBarcodeLookupErrorFailsClosed(t *testing.T) {
	ctx := context.Background()
	repo := openTestDB(t, filepath.Join(t.TempDir(), "metadata.db"))
	_ = repo.db.Close()

	cartridge := domain.NewVirtualCartridge("cart-new", "pool-a", "lib-a", "VTA123L06", 1024)
	if err := repo.CreateCartridge(ctx, cartridge); err == nil {
		t.Fatal("expected closed database error to fail cartridge create")
	}
}

func TestCoreResourcesRepoCreateOnlyRejectsDuplicates(t *testing.T) {
	ctx := context.Background()
	repo := openTestDB(t, filepath.Join(t.TempDir(), "metadata.db"))
	lib, _ := domain.NewVirtualLibrary("lib-a", "Library A")
	if err := repo.CreateLibrary(ctx, lib); err != nil {
		t.Fatalf("create first library: %v", err)
	}
	if err := repo.CreateLibrary(ctx, lib); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("expected duplicate library conflict, got %v", err)
	}
}

func TestCoreResourcesRepoEnforcesCartridgePoolForeignKey(t *testing.T) {
	ctx := context.Background()
	repo := openTestDB(t, filepath.Join(t.TempDir(), "metadata.db"))
	lib, _ := domain.NewVirtualLibrary("lib-a", "Library A")
	if err := repo.SaveLibrary(ctx, lib); err != nil {
		t.Fatalf("save library: %v", err)
	}
	cartridge := domain.NewVirtualCartridge("cart-a", "missing-pool", "lib-a", "VTA000L06", 1024)
	if err := repo.SaveCartridge(ctx, cartridge); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("expected missing pool conflict, got %v", err)
	}
}

func TestCoreResourcesRepoListReturnsIndependentValues(t *testing.T) {
	ctx := context.Background()
	repo := openTestDB(t, filepath.Join(t.TempDir(), "metadata.db"))
	lib, _ := domain.NewVirtualLibrary("lib-a", "Library A")
	if err := repo.SaveLibrary(ctx, lib); err != nil {
		t.Fatalf("save library: %v", err)
	}
	list := repo.ListLibraries(ctx)
	if len(list) != 1 {
		t.Fatalf("expected one library, got %d", len(list))
	}
	list[0].Name = "mutated"
	reloaded, err := repo.FindLibrary(ctx, "lib-a")
	if err != nil {
		t.Fatalf("find library: %v", err)
	}
	if reloaded.Name != "Library A" {
		t.Fatalf("expected stored library to remain unchanged, got %q", reloaded.Name)
	}
}
