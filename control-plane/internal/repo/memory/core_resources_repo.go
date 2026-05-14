package memory

import (
	"context"
	"sort"
	"strings"
	"sync"

	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
)

type CoreResourcesRepo struct {
	mu           sync.RWMutex
	libraries    map[string]*domain.VirtualLibrary
	drives       map[string]*domain.VirtualDrive
	cartridges   map[string]*domain.VirtualCartridge
	barcodeIndex map[string]string
	destroyed    map[string]struct{}
}

func NewCoreResourcesRepo() *CoreResourcesRepo {
	return &CoreResourcesRepo{
		libraries:    make(map[string]*domain.VirtualLibrary),
		drives:       make(map[string]*domain.VirtualDrive),
		cartridges:   make(map[string]*domain.VirtualCartridge),
		barcodeIndex: make(map[string]string),
		destroyed:    make(map[string]struct{}),
	}
}

func (r *CoreResourcesRepo) CreateLibrary(_ context.Context, library *domain.VirtualLibrary) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if library == nil {
		return domain.ErrInvalidInput
	}
	if _, exists := r.libraries[library.LibraryID]; exists {
		return domain.ErrConflict
	}
	r.libraries[library.LibraryID] = cloneLibrary(library)
	return nil
}

func (r *CoreResourcesRepo) SaveLibrary(_ context.Context, library *domain.VirtualLibrary) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.libraries[library.LibraryID] = cloneLibrary(library)
	return nil
}

func (r *CoreResourcesRepo) CreateDrive(_ context.Context, drive *domain.VirtualDrive) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if drive == nil {
		return domain.ErrInvalidInput
	}
	if _, exists := r.drives[drive.DriveID]; exists {
		return domain.ErrConflict
	}
	r.drives[drive.DriveID] = cloneDrive(drive)
	return nil
}

func (r *CoreResourcesRepo) SaveDrive(_ context.Context, drive *domain.VirtualDrive) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.drives[drive.DriveID] = cloneDrive(drive)
	return nil
}

func (r *CoreResourcesRepo) CreateCartridge(_ context.Context, c *domain.VirtualCartridge) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c == nil {
		return domain.ErrInvalidInput
	}
	if _, destroyed := r.destroyed[normalizeBarcodeKey(c.Barcode)]; destroyed {
		return domain.ErrConflict
	}
	if _, exists := r.cartridges[c.CartridgeID]; exists {
		return domain.ErrConflict
	}
	barcodeKey := normalizeBarcodeKey(c.Barcode)
	if _, exists := r.barcodeIndex[barcodeKey]; exists {
		return domain.ErrConflict
	}
	r.cartridges[c.CartridgeID] = cloneCartridge(c)
	r.barcodeIndex[barcodeKey] = c.CartridgeID
	return nil
}

func (r *CoreResourcesRepo) SaveCartridge(_ context.Context, c *domain.VirtualCartridge) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	barcodeKey := normalizeBarcodeKey(c.Barcode)
	if _, ok := r.destroyed[barcodeKey]; ok {
		return domain.ErrConflict
	}
	if existingID, ok := r.barcodeIndex[barcodeKey]; ok && existingID != c.CartridgeID {
		return domain.ErrConflict
	}
	if existing := r.cartridges[c.CartridgeID]; existing != nil {
		oldKey := normalizeBarcodeKey(existing.Barcode)
		if oldKey != "" && oldKey != barcodeKey {
			delete(r.barcodeIndex, oldKey)
		}
	}
	r.cartridges[c.CartridgeID] = cloneCartridge(c)
	r.barcodeIndex[barcodeKey] = c.CartridgeID
	return nil
}

func (r *CoreResourcesRepo) RetireCartridgeBarcode(_ context.Context, barcode, cartridgeID, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	barcodeKey := normalizeBarcodeKey(barcode)
	if barcodeKey == "" || strings.TrimSpace(cartridgeID) == "" {
		return domain.ErrInvalidInput
	}
	r.destroyed[barcodeKey] = struct{}{}
	return nil
}

func (r *CoreResourcesRepo) DestroyCartridge(_ context.Context, cartridgeID, barcode, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	barcodeKey := normalizeBarcodeKey(barcode)
	if barcodeKey == "" || strings.TrimSpace(cartridgeID) == "" {
		return domain.ErrInvalidInput
	}
	if existing, ok := r.cartridges[cartridgeID]; ok {
		delete(r.barcodeIndex, normalizeBarcodeKey(existing.Barcode))
	} else {
		return domain.ErrNotFound
	}
	r.destroyed[barcodeKey] = struct{}{}
	delete(r.cartridges, cartridgeID)
	return nil
}

func (r *CoreResourcesRepo) ListRetiredCartridgeBarcodes(_ context.Context) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.destroyed))
	for barcode := range r.destroyed {
		out = append(out, barcode)
	}
	sort.Strings(out)
	return out
}

func (r *CoreResourcesRepo) DeleteCartridge(_ context.Context, cartridgeID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	existing, ok := r.cartridges[cartridgeID]
	if !ok {
		return domain.ErrNotFound
	}
	delete(r.barcodeIndex, normalizeBarcodeKey(existing.Barcode))
	delete(r.cartridges, cartridgeID)
	return nil
}

func (r *CoreResourcesRepo) DeleteDrive(_ context.Context, driveID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.drives[driveID]; !ok {
		return domain.ErrNotFound
	}
	delete(r.drives, driveID)
	return nil
}

func (r *CoreResourcesRepo) DeleteLibrary(_ context.Context, libraryID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.libraries[libraryID]; !ok {
		return domain.ErrNotFound
	}
	delete(r.libraries, libraryID)
	return nil
}

func (r *CoreResourcesRepo) FindLibrary(_ context.Context, libraryID string) (*domain.VirtualLibrary, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	lib, ok := r.libraries[libraryID]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return cloneLibrary(lib), nil
}

func (r *CoreResourcesRepo) FindDrive(_ context.Context, driveID string) (*domain.VirtualDrive, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	d, ok := r.drives[driveID]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return cloneDrive(d), nil
}

func (r *CoreResourcesRepo) FindCartridge(_ context.Context, cartridgeID string) (*domain.VirtualCartridge, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.cartridges[cartridgeID]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return cloneCartridge(c), nil
}

func (r *CoreResourcesRepo) ListLibraries(_ context.Context) []*domain.VirtualLibrary {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*domain.VirtualLibrary, 0, len(r.libraries))
	for _, library := range r.libraries {
		out = append(out, cloneLibrary(library))
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].LibraryID < out[j].LibraryID
	})
	return out
}

func (r *CoreResourcesRepo) ListDrives(_ context.Context) []*domain.VirtualDrive {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*domain.VirtualDrive, 0, len(r.drives))
	for _, drive := range r.drives {
		out = append(out, cloneDrive(drive))
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].DriveID < out[j].DriveID
	})
	return out
}

func (r *CoreResourcesRepo) ListCartridges(_ context.Context) []*domain.VirtualCartridge {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*domain.VirtualCartridge, 0, len(r.cartridges))
	for _, cartridge := range r.cartridges {
		out = append(out, cloneCartridge(cartridge))
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CartridgeID < out[j].CartridgeID
	})
	return out
}

func cloneLibrary(in *domain.VirtualLibrary) *domain.VirtualLibrary {
	if in == nil {
		return nil
	}
	cp := *in
	return &cp
}

func cloneDrive(in *domain.VirtualDrive) *domain.VirtualDrive {
	if in == nil {
		return nil
	}
	cp := *in
	return &cp
}

func cloneCartridge(in *domain.VirtualCartridge) *domain.VirtualCartridge {
	if in == nil {
		return nil
	}
	cp := *in
	if in.CurrentElementAddress != nil {
		value := *in.CurrentElementAddress
		cp.CurrentElementAddress = &value
	}
	if in.AssignedSlotAddress != nil {
		value := *in.AssignedSlotAddress
		cp.AssignedSlotAddress = &value
	}
	return &cp
}

func normalizeBarcodeKey(raw string) string {
	return strings.ToUpper(strings.TrimSpace(raw))
}
