package memory

import (
	"context"
	"errors"
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
