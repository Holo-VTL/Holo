package orchestration

import (
	"errors"
	"testing"

	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
)

func TestValidateResourceIdentityBlocksHistoricalAliasesAndPreservesNormalResources(t *testing.T) {
	library, _ := domain.NewVirtualLibrary("library-a", "Library A")
	drive, _ := domain.NewVirtualDrive("drive name", library.LibraryID, 1)
	cartridge := domain.NewVirtualCartridge("cart.a", "pool.a", library.LibraryID, "VTA001L06", 1024)
	pool, _ := domain.NewStoragePoolRuntime("pool.a", "Pool A", 90)

	if err := ValidateResourceIdentity([]*domain.VirtualLibrary{library}, []*domain.VirtualDrive{drive}, []*domain.VirtualCartridge{cartridge}, []*domain.StoragePoolRuntime{pool}, library.LibraryID, drive.DriveID, cartridge.CartridgeID); err != nil {
		t.Fatalf("valid resource identity should pass: %v", err)
	}

	aliasedCartridge := domain.NewVirtualCartridge("cart_a", "pool.a", library.LibraryID, "VTA002L06", 1024)
	if err := ValidateResourceIdentity([]*domain.VirtualLibrary{library}, []*domain.VirtualDrive{drive}, []*domain.VirtualCartridge{cartridge, aliasedCartridge}, []*domain.StoragePoolRuntime{pool}, library.LibraryID, drive.DriveID, cartridge.CartridgeID); !errors.Is(err, domain.ErrIdentityConflict) {
		t.Fatalf("expected historical cartridge metadata alias conflict, got %v", err)
	}

	aliasedDrive, _ := domain.NewVirtualDrive("drive-name", library.LibraryID, 2)
	if drive.IQN != aliasedDrive.IQN {
		t.Fatal("test setup requires a generated IQN alias")
	}
	if err := ValidateResourceIdentity([]*domain.VirtualLibrary{library}, []*domain.VirtualDrive{drive, aliasedDrive}, []*domain.VirtualCartridge{cartridge}, []*domain.StoragePoolRuntime{pool}, library.LibraryID, drive.DriveID, cartridge.CartridgeID); !errors.Is(err, domain.ErrIdentityConflict) {
		t.Fatalf("expected historical target IQN conflict, got %v", err)
	}

	aliasedPool, _ := domain.NewStoragePoolRuntime("pool_a", "Pool Alias", 90)
	if err := ValidateResourceIdentity([]*domain.VirtualLibrary{library}, []*domain.VirtualDrive{drive}, []*domain.VirtualCartridge{cartridge}, []*domain.StoragePoolRuntime{pool, aliasedPool}, library.LibraryID, drive.DriveID, cartridge.CartridgeID); !errors.Is(err, domain.ErrIdentityConflict) {
		t.Fatalf("expected historical pool root conflict, got %v", err)
	}
}

func TestValidateResourceIdentityBlocksMediaStateSeparatorAlias(t *testing.T) {
	firstLibrary, _ := domain.NewVirtualLibrary("a", "Library A")
	secondLibrary, _ := domain.NewVirtualLibrary("a__b", "Library B")
	firstDrive, _ := domain.NewVirtualDrive("b__x", firstLibrary.LibraryID, 1)
	secondDrive, _ := domain.NewVirtualDrive("x", secondLibrary.LibraryID, 2)
	cartridge := domain.NewVirtualCartridge("VTA001L06", "pool-a", firstLibrary.LibraryID, "VTA001L06", 1024)
	if firstDrive.IQN == secondDrive.IQN {
		t.Fatal("test identities unexpectedly share a target IQN")
	}
	err := ValidateResourceIdentity(
		[]*domain.VirtualLibrary{firstLibrary, secondLibrary},
		[]*domain.VirtualDrive{firstDrive, secondDrive},
		[]*domain.VirtualCartridge{cartridge}, nil,
		firstLibrary.LibraryID, firstDrive.DriveID, cartridge.CartridgeID,
	)
	if !errors.Is(err, domain.ErrIdentityConflict) {
		t.Fatalf("expected shared media state path conflict, got %v", err)
	}
}
