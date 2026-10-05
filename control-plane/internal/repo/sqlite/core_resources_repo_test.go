package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
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

func TestSQLiteCoreResourcesRepoRejectsProjectedIdentityAliases(t *testing.T) {
	ctx := context.Background()
	repo := openTestDB(t, filepath.Join(t.TempDir(), "metadata.db"))

	firstLibrary, _ := domain.NewVirtualLibrary("lib.a", "Library A")
	secondLibrary, _ := domain.NewVirtualLibrary("lib_a", "Library B")
	if err := repo.CreateLibrary(ctx, firstLibrary); err != nil {
		t.Fatalf("create first projected library: %v", err)
	}
	if err := repo.CreateLibrary(ctx, secondLibrary); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("expected aliased library directory conflict, got %v", err)
	}

	library, _ := domain.NewVirtualLibrary("lib-drive", "Drive Library")
	if err := repo.CreateLibrary(ctx, library); err != nil {
		t.Fatal(err)
	}
	firstDrive, _ := domain.NewVirtualDrive("drive name", library.LibraryID, 1)
	secondDrive, _ := domain.NewVirtualDrive("drive-name", library.LibraryID, 2)
	if firstDrive.IQN != secondDrive.IQN {
		t.Fatalf("test requires generated IQN alias, got %q and %q", firstDrive.IQN, secondDrive.IQN)
	}
	if err := repo.CreateDrive(ctx, firstDrive); err != nil {
		t.Fatalf("create first drive: %v", err)
	}
	if err := repo.CreateDrive(ctx, secondDrive); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("expected generated IQN conflict, got %v", err)
	}

	driveAliasLibrary, _ := domain.NewVirtualLibrary("lib-drive-alias", "Drive Alias Library")
	if err := repo.CreateLibrary(ctx, driveAliasLibrary); err != nil {
		t.Fatal(err)
	}
	firstAliasDrive, _ := domain.NewVirtualDrive("drive.a", driveAliasLibrary.LibraryID, 1)
	secondAliasDrive, _ := domain.NewVirtualDrive("drive_a", driveAliasLibrary.LibraryID, 2)
	if firstAliasDrive.IQN == secondAliasDrive.IQN {
		t.Fatal("test identities unexpectedly share a target IQN")
	}
	if err := repo.CreateDrive(ctx, firstAliasDrive); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateDrive(ctx, secondAliasDrive); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("expected aliased legacy drive directory conflict, got %v", err)
	}

	firstStateLibrary, _ := domain.NewVirtualLibrary("a", "State A")
	secondStateLibrary, _ := domain.NewVirtualLibrary("a__b", "State B")
	if err := repo.CreateLibrary(ctx, firstStateLibrary); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateLibrary(ctx, secondStateLibrary); err != nil {
		t.Fatal(err)
	}
	firstStateDrive, _ := domain.NewVirtualDrive("b__x", "a", 1)
	secondStateDrive, _ := domain.NewVirtualDrive("x", "a__b", 2)
	if firstStateDrive.IQN == secondStateDrive.IQN {
		t.Fatal("test identities unexpectedly share a target IQN")
	}
	if err := repo.CreateDrive(ctx, firstStateDrive); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateDrive(ctx, secondStateDrive); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("expected shared media-state path conflict, got %v", err)
	}

	if err := NewStoragePoolRepo(repo.db).SavePool(ctx, mustSQLiteStoragePool(t, "pool-a")); err != nil {
		t.Fatalf("save pool: %v", err)
	}
	firstCartridge := domain.NewVirtualCartridge("cart.a", "pool-a", library.LibraryID, "VTA001L06", 1024)
	secondCartridge := domain.NewVirtualCartridge("cart_a", "pool-a", library.LibraryID, "VTA002L06", 1024)
	if err := repo.CreateCartridge(ctx, firstCartridge); err != nil {
		t.Fatalf("create first projected cartridge: %v", err)
	}
	if err := repo.CreateCartridge(ctx, secondCartridge); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("expected aliased cartridge metadata conflict, got %v", err)
	}

	firstDrive.Slot = 3
	if err := repo.SaveDrive(ctx, firstDrive); err != nil {
		t.Fatalf("save same drive after non-identity update: %v", err)
	}
}

func TestSQLiteCoreResourcesRepoPreservesHistoricalProjectionConflicts(t *testing.T) {
	ctx := context.Background()
	repo := openTestDB(t, filepath.Join(t.TempDir(), "metadata.db"))
	first, _ := domain.NewVirtualLibrary("lib.a", "Original A")
	if err := repo.CreateLibrary(ctx, first); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.Exec(`INSERT INTO virtual_libraries (library_id, name, status, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`, "lib_a", "Historical B", "ready", "created", "updated"); err != nil {
		t.Fatalf("insert historical alias fixture: %v", err)
	}
	first.Name = "Attempted Update"
	if err := repo.SaveLibrary(ctx, first); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("expected conflict to block update to a historically aliased library, got %v", err)
	}
	var firstName, secondName string
	if err := repo.db.QueryRow(`SELECT name FROM virtual_libraries WHERE library_id='lib.a'`).Scan(&firstName); err != nil {
		t.Fatal(err)
	}
	if err := repo.db.QueryRow(`SELECT name FROM virtual_libraries WHERE library_id='lib_a'`).Scan(&secondName); err != nil {
		t.Fatal(err)
	}
	if firstName != "Original A" || secondName != "Historical B" {
		t.Fatalf("historical rows changed: first=%q second=%q", firstName, secondName)
	}
}

func TestSQLiteCoreResourcesRepoSerializesProjectedLibraryCollisionsAcrossConnections(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "metadata.db")
	repos := []*CoreResourcesRepo{openTestDB(t, path), openTestDB(t, path)}
	ids := []string{"concurrent.lib", "concurrent_lib"}
	start := make(chan struct{})
	results := make(chan error, len(repos))
	var workers sync.WaitGroup
	for index, repo := range repos {
		id := ids[index]
		workers.Add(1)
		go func(repo *CoreResourcesRepo, id string) {
			defer workers.Done()
			library, _ := domain.NewVirtualLibrary(id, id)
			<-start
			results <- repo.CreateLibrary(ctx, library)
		}(repo, id)
	}
	close(start)
	workers.Wait()
	close(results)
	created, conflicts := 0, 0
	for err := range results {
		switch {
		case err == nil:
			created++
		case errors.Is(err, domain.ErrConflict):
			conflicts++
		default:
			t.Fatalf("unexpected concurrent create error: %v", err)
		}
	}
	if created != 1 || conflicts != 1 {
		t.Fatalf("expected one library and one conflict, created=%d conflicts=%d", created, conflicts)
	}
}

func mustSQLiteStoragePool(t *testing.T, id string) *domain.StoragePoolRuntime {
	t.Helper()
	pool, err := domain.NewStoragePoolRuntime(id, id, 90)
	if err != nil {
		t.Fatalf("create test storage pool: %v", err)
	}
	return pool
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
