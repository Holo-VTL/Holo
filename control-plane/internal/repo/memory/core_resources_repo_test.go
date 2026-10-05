package memory

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
)

func TestCoreResourcesRepoRejectsDestroyedBarcodeReuse(t *testing.T) {
	ctx := context.Background()
	repo := NewCoreResourcesRepo()

	if err := repo.RetireCartridgeBarcode(ctx, "VTA123L06", "cart-old", "tester"); err != nil {
		t.Fatalf("retire barcode: %v", err)
	}

	cartridge := domain.NewVirtualCartridge("cart-new", "pool-a", "lib-a", "vta123l06", 1024)
	if err := repo.SaveCartridge(ctx, cartridge); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("expected destroyed barcode conflict, got %v", err)
	}
}

func TestCoreResourcesRepoRejectsProjectedIdentityAliases(t *testing.T) {
	ctx := context.Background()
	repo := NewCoreResourcesRepo()

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
	firstDrive, _ := domain.NewVirtualDrive("drive.a", library.LibraryID, 1)
	secondDrive, _ := domain.NewVirtualDrive("drive_a", library.LibraryID, 2)
	if err := repo.CreateDrive(ctx, firstDrive); err != nil {
		t.Fatalf("create first projected drive: %v", err)
	}
	if err := repo.CreateDrive(ctx, secondDrive); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("expected aliased drive state path conflict, got %v", err)
	}

	firstCartridge := domain.NewVirtualCartridge("cart.a", "pool-a", library.LibraryID, "VTA001L06", 1024)
	secondCartridge := domain.NewVirtualCartridge("cart_a", "pool-a", library.LibraryID, "VTA002L06", 1024)
	if err := repo.CreateCartridge(ctx, firstCartridge); err != nil {
		t.Fatalf("create first projected cartridge: %v", err)
	}
	if err := repo.CreateCartridge(ctx, secondCartridge); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("expected aliased cartridge metadata conflict, got %v", err)
	}
}

func TestCoreResourcesRepoRejectsGeneratedIQNAliasesAndAllowsSameObjectUpdate(t *testing.T) {
	ctx := context.Background()
	repo := NewCoreResourcesRepo()
	library, _ := domain.NewVirtualLibrary("lib-drive-iqn", "Drive Library")
	if err := repo.CreateLibrary(ctx, library); err != nil {
		t.Fatal(err)
	}

	first, _ := domain.NewVirtualDrive("drive name", library.LibraryID, 1)
	second, _ := domain.NewVirtualDrive("drive-name", library.LibraryID, 2)
	if first.IQN != second.IQN {
		t.Fatalf("test requires generated IQN alias, got %q and %q", first.IQN, second.IQN)
	}
	if err := repo.CreateDrive(ctx, first); err != nil {
		t.Fatalf("create first drive: %v", err)
	}
	if err := repo.CreateDrive(ctx, second); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("expected generated IQN conflict, got %v", err)
	}

	first.Slot = 3
	if err := repo.SaveDrive(ctx, first); err != nil {
		t.Fatalf("save the same drive after a non-identity update: %v", err)
	}
}

func TestCoreResourcesRepoRejectsMediaStatePairAlias(t *testing.T) {
	ctx := context.Background()
	repo := NewCoreResourcesRepo()
	first, _ := domain.NewVirtualDrive("b__x", "a", 1)
	second, _ := domain.NewVirtualDrive("x", "a__b", 2)
	if first.IQN == second.IQN || first.DriveID == second.DriveID {
		t.Fatal("test identities unexpectedly collide outside the media-state projection")
	}
	if err := repo.CreateDrive(ctx, first); err != nil {
		t.Fatalf("create first drive: %v", err)
	}
	if err := repo.CreateDrive(ctx, second); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("expected shared media-state path conflict, got %v", err)
	}
}

func TestCoreResourcesRepoSerializesProjectedLibraryCollisions(t *testing.T) {
	ctx := context.Background()
	repo := NewCoreResourcesRepo()
	ids := []string{"concurrent.lib", "concurrent_lib"}
	start := make(chan struct{})
	results := make(chan error, len(ids))
	var workers sync.WaitGroup
	for _, id := range ids {
		workers.Add(1)
		go func(id string) {
			defer workers.Done()
			library, _ := domain.NewVirtualLibrary(id, id)
			<-start
			results <- repo.CreateLibrary(ctx, library)
		}(id)
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
		t.Fatalf("expected one creation and one conflict, created=%d conflicts=%d", created, conflicts)
	}
}

func TestCoreResourcesRepoCreateOnlyRejectsDuplicates(t *testing.T) {
	ctx := context.Background()
	repo := NewCoreResourcesRepo()
	lib, _ := domain.NewVirtualLibrary("lib-a", "Library A")
	if err := repo.CreateLibrary(ctx, lib); err != nil {
		t.Fatalf("create first library: %v", err)
	}
	if err := repo.CreateLibrary(ctx, lib); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("expected duplicate library conflict, got %v", err)
	}
}

func TestCoreResourcesRepoMaintainsActiveBarcodeIndex(t *testing.T) {
	ctx := context.Background()
	repo := NewCoreResourcesRepo()

	first := domain.NewVirtualCartridge("cart-1", "pool-a", "lib-a", "VTA001L06", 1024)
	if err := repo.CreateCartridge(ctx, first); err != nil {
		t.Fatalf("create first cartridge: %v", err)
	}

	duplicate := domain.NewVirtualCartridge("cart-2", "pool-a", "lib-a", "vta001l06", 1024)
	if err := repo.CreateCartridge(ctx, duplicate); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("expected duplicate barcode conflict, got %v", err)
	}

	first.Barcode = "VTA002L06"
	if err := repo.SaveCartridge(ctx, first); err != nil {
		t.Fatalf("save changed barcode: %v", err)
	}
	newDuplicate := domain.NewVirtualCartridge("cart-3", "pool-a", "lib-a", "vta002l06", 1024)
	if err := repo.CreateCartridge(ctx, newDuplicate); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("expected changed barcode duplicate conflict, got %v", err)
	}

	reuseOld := domain.NewVirtualCartridge("cart-2", "pool-a", "lib-a", "VTA001L06", 1024)
	if err := repo.CreateCartridge(ctx, reuseOld); err != nil {
		t.Fatalf("expected deleted active barcode to be reusable after save, got %v", err)
	}

	if err := repo.DeleteCartridge(ctx, "cart-2"); err != nil {
		t.Fatalf("delete cartridge: %v", err)
	}
	reuseDeleted := domain.NewVirtualCartridge("cart-2b", "pool-a", "lib-a", "vta001l06", 1024)
	if err := repo.CreateCartridge(ctx, reuseDeleted); err != nil {
		t.Fatalf("expected deleted active barcode to be reusable, got %v", err)
	}
	if err := repo.DestroyCartridge(ctx, "cart-1", "VTA002L06", "tester"); err != nil {
		t.Fatalf("destroy cartridge: %v", err)
	}
	destroyedReuse := domain.NewVirtualCartridge("cart-3", "pool-a", "lib-a", "vta002l06", 1024)
	if err := repo.CreateCartridge(ctx, destroyedReuse); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("expected destroyed barcode conflict, got %v", err)
	}
}
