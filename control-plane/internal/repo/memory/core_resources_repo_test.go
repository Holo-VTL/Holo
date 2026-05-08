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
