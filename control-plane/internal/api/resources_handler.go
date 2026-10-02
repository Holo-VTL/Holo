package api

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Holo-VTL/Holo/control-plane/internal/audit"
	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
	"github.com/Holo-VTL/Holo/control-plane/internal/orchestration"
	"github.com/Holo-VTL/Holo/control-plane/internal/storageutil"
	"github.com/Holo-VTL/Holo/control-plane/internal/tracing"
)

type ResourcesHandler struct {
	repo        coreResourcesRepo
	storage     resourceStoragePoolService
	target      resourceTargetService
	auditW      audit.Writer
	slotLocksMu sync.Mutex
	slotLocks   map[string]*sync.Mutex
}

type coreResourcesRepo interface {
	CreateLibrary(ctx context.Context, library *domain.VirtualLibrary) error
	CreateDrive(ctx context.Context, drive *domain.VirtualDrive) error
	CreateCartridge(ctx context.Context, cartridge *domain.VirtualCartridge) error
	SaveLibrary(ctx context.Context, library *domain.VirtualLibrary) error
	SaveDrive(ctx context.Context, drive *domain.VirtualDrive) error
	SaveCartridge(ctx context.Context, cartridge *domain.VirtualCartridge) error
	DeleteCartridge(ctx context.Context, cartridgeID string) error
	RetireCartridgeBarcode(ctx context.Context, barcode, cartridgeID, actor string) error
	DestroyCartridge(ctx context.Context, cartridgeID, barcode, actor string) error
	ListRetiredCartridgeBarcodes(ctx context.Context) []string
	DeleteDrive(ctx context.Context, driveID string) error
	DeleteLibrary(ctx context.Context, libraryID string) error
	FindLibrary(ctx context.Context, libraryID string) (*domain.VirtualLibrary, error)
	FindDrive(ctx context.Context, driveID string) (*domain.VirtualDrive, error)
	FindCartridge(ctx context.Context, cartridgeID string) (*domain.VirtualCartridge, error)
	ListLibraries(ctx context.Context) []*domain.VirtualLibrary
	ListDrives(ctx context.Context) []*domain.VirtualDrive
	ListCartridges(ctx context.Context) []*domain.VirtualCartridge
}

type resourceStoragePoolService interface {
	CreatePool(ctx context.Context, req orchestration.CreateStoragePoolRequest) (*domain.StoragePoolRuntime, error)
	GetPool(ctx context.Context, poolID string) (*domain.StoragePoolRuntime, error)
	ReconcilePoolUsedBytes(ctx context.Context, poolID string, usedBytes int64) error
}

type resourceTargetService interface {
	Publish(ctx context.Context, req orchestration.PublishRequest) (*domain.TargetPublication, error)
	ListPublications(ctx context.Context) []*domain.TargetPublication
	Unpublish(ctx context.Context, publicationID, actor string) (*domain.TargetPublication, error)
}

type resourceLocalMountSync interface {
	SyncLocalMountAsync(string)
}

func NewResourcesHandler(repo coreResourcesRepo, storage resourceStoragePoolService, target resourceTargetService) *ResourcesHandler {
	return &ResourcesHandler{repo: repo, storage: storage, target: target}
}

func NewResourcesHandlerWithAudit(repo coreResourcesRepo, storage resourceStoragePoolService, target resourceTargetService, auditW audit.Writer) *ResourcesHandler {
	return &ResourcesHandler{repo: repo, storage: storage, target: target, auditW: auditW}
}

func (h *ResourcesHandler) lockLibrarySlots(libraryID string) func() {
	key := strings.TrimSpace(libraryID)
	if key == "" {
		key = "__unknown_library__"
	}
	h.slotLocksMu.Lock()
	if h.slotLocks == nil {
		h.slotLocks = make(map[string]*sync.Mutex)
	}
	mu := h.slotLocks[key]
	if mu == nil {
		mu = &sync.Mutex{}
		h.slotLocks[key] = mu
	}
	h.slotLocksMu.Unlock()
	mu.Lock()
	return mu.Unlock
}

func (h *ResourcesHandler) syncLocalMount(actor string) {
	if syncer, ok := h.target.(resourceLocalMountSync); ok {
		syncer.SyncLocalMountAsync(actor)
	}
}

func (h *ResourcesHandler) logCompensationError(ctx context.Context, operation string, err error, fields ...any) {
	if err == nil {
		return
	}
	tracing.LogError(ctx, "resources", "compensation cleanup failed", err, append([]any{"operation", operation}, fields...)...)
}

const (
	maxLibraryDriveCount = 4
	maxLibrarySlotCount  = 10000
	defaultLibrarySlots  = 20
)

type createLibraryRequest struct {
	LibraryID          string `json:"libraryId"`
	PoolID             string `json:"poolId,omitempty"`
	Name               string `json:"name"`
	Vendor             string `json:"vendor,omitempty"`
	LibraryType        string `json:"libraryType,omitempty"`
	DriveType          string `json:"driveType,omitempty"`
	DriveCount         int    `json:"driveCount,omitempty"`
	DriveStartAddress  int    `json:"driveStartAddress,omitempty"`
	SlotCount          int    `json:"slotCount,omitempty"`
	SlotStartAddress   int    `json:"slotStartAddress,omitempty"`
	IEPortCount        int    `json:"iePortCount,omitempty"`
	IEStartAddress     int    `json:"ieStartAddress,omitempty"`
	CompressionEnabled *bool  `json:"compressionEnabled,omitempty"`
	DedupEnabled       *bool  `json:"dedupEnabled,omitempty"`
}

type createDriveRequest struct {
	DriveID   string `json:"driveId"`
	LibraryID string `json:"libraryId"`
	Slot      int    `json:"slot"`
}

type createCartridgeRequest struct {
	CartridgeID   string `json:"cartridgeId"`
	PoolID        string `json:"poolId"`
	LibraryID     string `json:"libraryId"`
	Barcode       string `json:"barcode"`
	BarcodePrefix string `json:"barcodePrefix,omitempty"`
	CapacityBytes int64  `json:"capacityBytes"`
	LTOGeneration int    `json:"ltoGeneration,omitempty"`
	MediaType     string `json:"mediaType,omitempty"`
	ExpandSlots   bool   `json:"expandSlots,omitempty"`
}

type loadDriveRequest struct {
	CartridgeID string `json:"cartridgeId"`
	Actor       string `json:"actor,omitempty"`
}

type addSlotsRequest struct {
	Count int    `json:"count,omitempty"`
	Actor string `json:"actor,omitempty"`
}

type resourceActorRequest struct {
	Actor string `json:"actor,omitempty"`
}

type eraseCartridgeRequest struct {
	Mode  string `json:"mode"`
	Actor string `json:"actor,omitempty"`
}

type resourceChainRequest struct {
	PoolID        string `json:"poolId"`
	PoolName      string `json:"poolName"`
	CapacityBytes int64  `json:"capacityBytes,omitempty"`
	LibraryID     string `json:"libraryId"`
	LibraryName   string `json:"libraryName"`
	DriveID       string `json:"driveId"`
	DriveSlot     int    `json:"driveSlot"`
	CartridgeID   string `json:"cartridgeId"`
	Barcode       string `json:"barcode"`
}

type resourceChainResponse struct {
	Pool      *domain.StoragePool      `json:"pool"`
	Library   *domain.VirtualLibrary   `json:"library"`
	Drive     *domain.VirtualDrive     `json:"drive"`
	Cartridge *domain.VirtualCartridge `json:"cartridge"`
}

func (h *ResourcesHandler) handleLibraries(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		var req createLibraryRequest
		if err := decodeRequiredJSONBody(r, &req); err != nil {
			respondResourceError(w, err)
			return
		}
		if validateManagementID(req.LibraryID) != nil || validateManagementLabel(req.Name, true) != nil {
			respondResourceError(w, domain.ErrInvalidInput)
			return
		}
		library, err := domain.NewVirtualLibrary(strings.TrimSpace(req.LibraryID), strings.TrimSpace(req.Name))
		if err != nil {
			respondResourceError(w, err)
			return
		}
		if req.DriveCount < 0 || req.DriveCount > maxLibraryDriveCount || req.DriveStartAddress < 0 || req.SlotCount < 0 || req.SlotCount > maxLibrarySlotCount || req.SlotStartAddress < 0 || req.IEPortCount < 0 || req.IEStartAddress < 0 {
			respondResourceError(w, domain.ErrInvalidInput)
			return
		}
		if validateManagementLabel(req.Vendor, false) != nil || validateProfileToken(req.LibraryType) != nil || validateProfileToken(req.DriveType) != nil {
			respondResourceError(w, domain.ErrInvalidInput)
			return
		}
		library.Vendor = strings.TrimSpace(req.Vendor)
		library.LibraryType = strings.TrimSpace(req.LibraryType)
		library.DriveType = strings.TrimSpace(req.DriveType)
		library.DriveCount = req.DriveCount
		library.DriveStartAddress = req.DriveStartAddress
		library.SlotCount = req.SlotCount
		if library.SlotCount == 0 {
			library.SlotCount = defaultLibrarySlots
		}
		library.SlotStartAddress = req.SlotStartAddress
		library.IEPortCount = req.IEPortCount
		library.IEStartAddress = req.IEStartAddress
		if req.CompressionEnabled != nil {
			library.CompressionEnabled = *req.CompressionEnabled
		}
		if req.DedupEnabled != nil {
			library.DedupEnabled = *req.DedupEnabled
		}
		if err := h.repo.CreateLibrary(r.Context(), library); err != nil {
			respondResourceError(w, err)
			return
		}
		h.syncLocalMount("web-console")
		respondJSON(w, http.StatusCreated, library)
	case http.MethodGet:
		respondJSON(w, http.StatusOK, h.repo.ListLibraries(r.Context()))
	default:
		respondError(w, http.StatusMethodNotAllowed, "method not allowed", nil)
	}
}

func (h *ResourcesHandler) handleLibraryByID(w http.ResponseWriter, r *http.Request) {
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/libraries/"), "/")
	if path == "" {
		respondError(w, http.StatusBadRequest, "invalid request", domain.ErrInvalidInput)
		return
	}
	parts := strings.Split(path, "/")
	libraryID := strings.TrimSpace(parts[0])
	if libraryID == "" {
		respondError(w, http.StatusBadRequest, "invalid request", domain.ErrInvalidInput)
		return
	}

	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			library, err := h.repo.FindLibrary(r.Context(), libraryID)
			if err != nil {
				respondResourceError(w, err)
				return
			}
			respondJSON(w, http.StatusOK, library)
		case http.MethodDelete:
			if err := h.deleteLibraryCascade(r.Context(), libraryID); err != nil {
				respondResourceError(w, err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			respondError(w, http.StatusMethodNotAllowed, "method not allowed", nil)
		}
		return
	}

	if len(parts) == 2 && parts[1] == "delete" {
		if r.Method != http.MethodPost {
			respondError(w, http.StatusMethodNotAllowed, "method not allowed", nil)
			return
		}
		if err := h.deleteLibraryCascade(r.Context(), libraryID); err != nil {
			respondResourceError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if len(parts) == 2 && parts[1] == "slots" {
		if r.Method != http.MethodPost {
			respondError(w, http.StatusMethodNotAllowed, "method not allowed", nil)
			return
		}
		var req addSlotsRequest
		if err := decodeOptionalJSONBody(r, &req); err != nil {
			respondResourceError(w, err)
			return
		}
		actor, err := selfAssertedAuditActor(req.Actor)
		if err != nil {
			respondResourceError(w, err)
			return
		}
		library, err := h.addLibrarySlots(r.Context(), libraryID, req.Count, actor)
		if err != nil {
			respondResourceError(w, err)
			return
		}
		respondJSON(w, http.StatusOK, library)
		return
	}

	respondError(w, http.StatusNotFound, "not found", nil)
}

func (h *ResourcesHandler) handleDrives(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		var req createDriveRequest
		if err := decodeRequiredJSONBody(r, &req); err != nil {
			respondResourceError(w, err)
			return
		}
		if validateManagementID(req.DriveID) != nil || validateManagementID(req.LibraryID) != nil {
			respondResourceError(w, domain.ErrInvalidInput)
			return
		}
		drive, err := domain.NewVirtualDrive(strings.TrimSpace(req.DriveID), strings.TrimSpace(req.LibraryID), req.Slot)
		if err != nil {
			respondResourceError(w, err)
			return
		}
		if _, err := h.repo.FindLibrary(r.Context(), drive.LibraryID); err != nil {
			respondResourceError(w, err)
			return
		}
		if h.wouldExceedLibraryDriveLimit(r.Context(), drive.LibraryID, drive.DriveID) {
			respondResourceError(w, domain.ErrInvalidInput)
			return
		}
		if err := h.repo.CreateDrive(r.Context(), drive); err != nil {
			respondResourceError(w, err)
			return
		}
		if err := h.syncLibrarySlotsToSharedState(r.Context(), drive.LibraryID); err != nil {
			h.logCompensationError(r.Context(), "delete drive after slot sync failure", h.repo.DeleteDrive(r.Context(), drive.DriveID), "driveId", drive.DriveID, "libraryId", drive.LibraryID)
			respondResourceError(w, err)
			return
		}
		if err := h.ensureLibraryAutoPublications(r.Context(), drive.LibraryID); err != nil {
			h.logCompensationError(r.Context(), "delete drive after publication failure", h.repo.DeleteDrive(r.Context(), drive.DriveID), "driveId", drive.DriveID, "libraryId", drive.LibraryID)
			h.logCompensationError(r.Context(), "remove drive media state after publication failure", removeDriveMediaStateFile(drive.LibraryID, drive.DriveID), "driveId", drive.DriveID, "libraryId", drive.LibraryID)
			h.logCompensationError(r.Context(), "resync library slots after publication failure", h.syncLibrarySlotsToSharedState(r.Context(), drive.LibraryID), "libraryId", drive.LibraryID)
			respondResourceError(w, err)
			return
		}
		respondJSON(w, http.StatusCreated, drive)
	case http.MethodGet:
		if err := h.reconcileMediaState(r.Context()); err != nil {
			respondResourceError(w, err)
			return
		}
		respondJSON(w, http.StatusOK, h.repo.ListDrives(r.Context()))
	default:
		respondError(w, http.StatusMethodNotAllowed, "method not allowed", nil)
	}
}

func (h *ResourcesHandler) handleDriveByID(w http.ResponseWriter, r *http.Request) {
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/drives/"), "/")
	if path == "" {
		respondError(w, http.StatusBadRequest, "invalid request", domain.ErrInvalidInput)
		return
	}
	parts := strings.Split(path, "/")
	driveID := strings.TrimSpace(parts[0])
	if driveID == "" {
		respondError(w, http.StatusBadRequest, "invalid request", domain.ErrInvalidInput)
		return
	}

	deleteDrive := func() {
		if err := h.reconcileMediaState(r.Context()); err != nil {
			respondResourceError(w, err)
			return
		}
		drive, err := h.repo.FindDrive(r.Context(), driveID)
		if err != nil {
			respondResourceError(w, err)
			return
		}
		if drive.MountedCartridgeID != "" {
			respondResourceError(w, domain.ErrInvalidState)
			return
		}
		if err := h.unpublishDependentPublications(r.Context(), "", driveID, ""); err != nil {
			respondResourceError(w, err)
			return
		}
		if err := h.repo.DeleteDrive(r.Context(), driveID); err != nil {
			respondResourceError(w, err)
			return
		}
		if err := removeDriveMediaStateFile(drive.LibraryID, drive.DriveID); err != nil {
			respondResourceError(w, err)
			return
		}
		if err := h.syncLibrarySlotsToSharedState(r.Context(), drive.LibraryID); err != nil {
			respondResourceError(w, err)
			return
		}
		if err := h.ensureLibraryAutoPublications(r.Context(), drive.LibraryID); err != nil {
			log.Printf("auto publication reconciliation failed after drive delete drive=%s library=%s err=%v", drive.DriveID, drive.LibraryID, err)
		}
		w.WriteHeader(http.StatusNoContent)
	}

	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			if err := h.reconcileMediaState(r.Context()); err != nil {
				respondResourceError(w, err)
				return
			}
			drive, err := h.repo.FindDrive(r.Context(), driveID)
			if err != nil {
				respondResourceError(w, err)
				return
			}
			respondJSON(w, http.StatusOK, drive)
		case http.MethodDelete:
			deleteDrive()
		default:
			respondError(w, http.StatusMethodNotAllowed, "method not allowed", nil)
		}
		return
	}

	if len(parts) == 2 && parts[1] == "delete" {
		if r.Method != http.MethodPost {
			respondError(w, http.StatusMethodNotAllowed, "method not allowed", nil)
			return
		}
		deleteDrive()
		return
	}

	if len(parts) == 2 && parts[1] == "load" {
		if r.Method != http.MethodPost {
			respondError(w, http.StatusMethodNotAllowed, "method not allowed", nil)
			return
		}
		var req loadDriveRequest
		if err := decodeRequiredJSONBody(r, &req); err != nil {
			respondResourceError(w, err)
			return
		}
		actor, err := selfAssertedAuditActor(req.Actor)
		if err != nil {
			respondResourceError(w, err)
			return
		}
		drive, err := h.loadCartridgeIntoDrive(r.Context(), driveID, req.CartridgeID, actor)
		if err != nil {
			respondResourceError(w, err)
			return
		}
		respondJSON(w, http.StatusOK, drive)
		return
	}

	if len(parts) == 2 && parts[1] == "unload" {
		if r.Method != http.MethodPost {
			respondError(w, http.StatusMethodNotAllowed, "method not allowed", nil)
			return
		}
		var req resourceActorRequest
		if err := decodeOptionalJSONBody(r, &req); err != nil {
			respondResourceError(w, err)
			return
		}
		actor, err := selfAssertedAuditActor(req.Actor)
		if err != nil {
			respondResourceError(w, err)
			return
		}
		drive, err := h.unloadDrive(r.Context(), driveID, actor)
		if err != nil {
			respondResourceError(w, err)
			return
		}
		respondJSON(w, http.StatusOK, drive)
		return
	}

	respondError(w, http.StatusNotFound, "not found", nil)
}

func (h *ResourcesHandler) handleCartridges(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		var req createCartridgeRequest
		if err := decodeRequiredJSONBody(r, &req); err != nil {
			respondResourceError(w, err)
			return
		}
		req.PoolID = strings.TrimSpace(req.PoolID)
		req.LibraryID = strings.TrimSpace(req.LibraryID)
		if req.LibraryID == "" || req.PoolID == "" || req.CapacityBytes <= 0 {
			respondResourceError(w, domain.ErrInvalidInput)
			return
		}
		if validateManagementID(req.LibraryID) != nil {
			respondResourceError(w, domain.ErrInvalidInput)
			return
		}
		if h.storage == nil {
			respondResourceError(w, domain.ErrInvalidState)
			return
		}
		pool, err := h.storage.GetPool(r.Context(), req.PoolID)
		if err != nil {
			respondResourceError(w, err)
			return
		}
		if storageutil.StrictStorageFlowEnabled() && len(pool.Disks) == 0 {
			respondResourceError(w, domain.ErrInvalidState)
			return
		}
		if _, err := h.repo.FindLibrary(r.Context(), req.LibraryID); err != nil {
			respondResourceError(w, err)
			return
		}
		unlock := h.lockLibrarySlots(req.LibraryID)
		defer unlock()
		library, err := h.repo.FindLibrary(r.Context(), req.LibraryID)
		if err != nil {
			respondResourceError(w, err)
			return
		}
		cartridgeID, barcode, err := h.resolveCartridgeIdentity(r.Context(), req)
		if err != nil {
			respondResourceError(w, err)
			return
		}
		cartridge := domain.NewVirtualCartridge(cartridgeID, req.PoolID, req.LibraryID, barcode, req.CapacityBytes)
		cartridge.UpdatedAt = time.Now().UTC()
		originalSlotCount := library.SlotCount
		slotAddress, expandedSlots, err := h.assignSlotForNewCartridge(r.Context(), library, req.ExpandSlots)
		if err != nil {
			respondResourceError(w, err)
			return
		}
		cartridge.AssignedSlotAddress = &slotAddress
		if err := h.repo.CreateCartridge(r.Context(), cartridge); err != nil {
			if expandedSlots {
				h.logCompensationError(r.Context(), "rollback slot expansion after cartridge create failure", h.rollbackLibrarySlotExpansion(r.Context(), cartridge.LibraryID, originalSlotCount), "cartridgeId", cartridge.CartridgeID, "libraryId", cartridge.LibraryID)
			}
			respondResourceError(w, err)
			return
		}
		if err := writeCartridgeMetadata(cartridge); err != nil {
			deleteErr := h.repo.DeleteCartridge(r.Context(), cartridge.CartridgeID)
			h.logCompensationError(r.Context(), "delete cartridge after metadata failure", deleteErr, "cartridgeId", cartridge.CartridgeID, "libraryId", cartridge.LibraryID)
			if expandedSlots && deleteErr == nil {
				h.logCompensationError(r.Context(), "rollback slot expansion after metadata failure", h.rollbackLibrarySlotExpansion(r.Context(), cartridge.LibraryID, originalSlotCount), "cartridgeId", cartridge.CartridgeID, "libraryId", cartridge.LibraryID)
			}
			respondResourceError(w, err)
			return
		}
		if err := h.syncLibrarySlotsToSharedState(r.Context(), cartridge.LibraryID); err != nil {
			deleteErr := h.repo.DeleteCartridge(r.Context(), cartridge.CartridgeID)
			h.logCompensationError(r.Context(), "delete cartridge after slot sync failure", deleteErr, "cartridgeId", cartridge.CartridgeID, "libraryId", cartridge.LibraryID)
			if expandedSlots && deleteErr == nil {
				h.logCompensationError(r.Context(), "rollback slot expansion after slot sync failure", h.rollbackLibrarySlotExpansion(r.Context(), cartridge.LibraryID, originalSlotCount), "cartridgeId", cartridge.CartridgeID, "libraryId", cartridge.LibraryID)
			}
			respondResourceError(w, err)
			return
		}
		if err := h.syncPoolUsage(r.Context(), cartridge.PoolID); err != nil {
			deleteErr := h.repo.DeleteCartridge(r.Context(), cartridge.CartridgeID)
			h.logCompensationError(r.Context(), "delete cartridge after pool usage sync failure", deleteErr, "poolId", cartridge.PoolID, "cartridgeId", cartridge.CartridgeID)
			if expandedSlots && deleteErr == nil {
				h.logCompensationError(r.Context(), "rollback slot expansion after pool usage sync failure", h.rollbackLibrarySlotExpansion(r.Context(), cartridge.LibraryID, originalSlotCount), "cartridgeId", cartridge.CartridgeID, "libraryId", cartridge.LibraryID)
			}
			h.logCompensationError(r.Context(), "resync library slots after pool usage sync failure", h.syncLibrarySlotsToSharedState(r.Context(), cartridge.LibraryID), "libraryId", cartridge.LibraryID)
			respondResourceError(w, err)
			return
		}
		if err := h.ensureLibraryAutoPublications(r.Context(), cartridge.LibraryID); err != nil {
			deleteErr := h.repo.DeleteCartridge(r.Context(), cartridge.CartridgeID)
			h.logCompensationError(r.Context(), "delete cartridge after publication failure", deleteErr, "cartridgeId", cartridge.CartridgeID, "libraryId", cartridge.LibraryID)
			if expandedSlots && deleteErr == nil {
				h.logCompensationError(r.Context(), "rollback slot expansion after publication failure", h.rollbackLibrarySlotExpansion(r.Context(), cartridge.LibraryID, originalSlotCount), "cartridgeId", cartridge.CartridgeID, "libraryId", cartridge.LibraryID)
			}
			h.logCompensationError(r.Context(), "resync library slots after publication failure", h.syncLibrarySlotsToSharedState(r.Context(), cartridge.LibraryID), "libraryId", cartridge.LibraryID)
			respondResourceError(w, err)
			return
		}
		if expandedSlots {
			updatedLibrary, findErr := h.repo.FindLibrary(r.Context(), library.LibraryID)
			if findErr == nil {
				h.emitLibraryAudit(r.Context(), "self-asserted:web-console", "library_add_slots", updatedLibrary, "success", map[string]any{
					"addedSlots":  updatedLibrary.SlotCount - originalSlotCount,
					"slotCount":   updatedLibrary.SlotCount,
					"reason":      "create_cartridge_expand_slots",
					"cartridgeId": cartridge.CartridgeID,
				})
			} else {
				log.Printf("library slot expansion audit skipped library=%s cartridge=%s err=%v", library.LibraryID, cartridge.CartridgeID, findErr)
			}
		}
		h.emitCartridgeAudit(r.Context(), "self-asserted:web-console", "cartridge_create", cartridge, "success", map[string]any{
			"assignedSlotAddress": slotAddress,
			"expandedSlots":       expandedSlots,
		})
		respondJSON(w, http.StatusCreated, cartridge)
	case http.MethodGet:
		if err := h.reconcileMediaState(r.Context()); err != nil {
			respondResourceError(w, err)
			return
		}
		cartridges := h.repo.ListCartridges(r.Context())
		h.annotateCartridgeElementAddresses(r.Context(), cartridges)
		respondJSON(w, http.StatusOK, cartridges)
	default:
		respondError(w, http.StatusMethodNotAllowed, "method not allowed", nil)
	}
}

func (h *ResourcesHandler) handleCartridgeByID(w http.ResponseWriter, r *http.Request) {
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/cartridges/"), "/")
	if path == "" {
		respondError(w, http.StatusBadRequest, "invalid request", domain.ErrInvalidInput)
		return
	}
	parts := strings.Split(path, "/")
	cartridgeID := strings.TrimSpace(parts[0])
	if cartridgeID == "" {
		respondError(w, http.StatusBadRequest, "invalid request", domain.ErrInvalidInput)
		return
	}

	deleteCartridge := func(actor string) {
		existing, err := h.repo.FindCartridge(r.Context(), cartridgeID)
		if err != nil {
			respondResourceError(w, err)
			return
		}
		unlock := h.lockLibrarySlots(existing.LibraryID)
		defer unlock()
		if err := h.reconcileMediaState(r.Context()); err != nil {
			respondResourceError(w, err)
			return
		}
		cartridge, err := h.repo.FindCartridge(r.Context(), cartridgeID)
		if err != nil {
			respondResourceError(w, err)
			return
		}
		for _, drive := range h.repo.ListDrives(r.Context()) {
			if drive != nil && strings.TrimSpace(drive.MountedCartridgeID) == cartridge.CartridgeID {
				respondResourceError(w, domain.ErrInvalidState)
				return
			}
		}
		if err := h.unpublishDependentPublications(r.Context(), "", "", cartridge.CartridgeID); err != nil {
			respondResourceError(w, err)
			return
		}
		if err := removeCartridgeLayoutArtifacts(cartridge); err != nil {
			respondResourceError(w, err)
			return
		}
		if err := removeCartridgeMetadataFile(cartridge); err != nil {
			respondResourceError(w, err)
			return
		}
		if err := h.repo.DestroyCartridge(r.Context(), cartridge.CartridgeID, cartridge.Barcode, nonEmpty(actor, "web-console")); err != nil {
			h.emitCartridgeAudit(r.Context(), nonEmpty(actor, "web-console"), "cartridge_destroy", cartridge, "failure", map[string]any{"reason": "destroy_record"})
			respondResourceError(w, err)
			return
		}
		if err := h.syncPoolUsage(r.Context(), cartridge.PoolID); err != nil {
			respondResourceError(w, err)
			return
		}
		if err := h.syncLibrarySlotsToSharedState(r.Context(), cartridge.LibraryID); err != nil {
			respondResourceError(w, err)
			return
		}
		if err := h.ensureLibraryAutoPublications(r.Context(), cartridge.LibraryID); err != nil {
			log.Printf("auto publication reconciliation failed after cartridge delete cartridge=%s library=%s err=%v", cartridge.CartridgeID, cartridge.LibraryID, err)
		}
		h.emitCartridgeAudit(r.Context(), nonEmpty(actor, "web-console"), "cartridge_destroy", cartridge, "success", map[string]any{"barcodeRetired": true})
		w.WriteHeader(http.StatusNoContent)
	}

	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			if err := h.reconcileMediaState(r.Context()); err != nil {
				respondResourceError(w, err)
				return
			}
			cartridge, err := h.repo.FindCartridge(r.Context(), cartridgeID)
			if err != nil {
				respondResourceError(w, err)
				return
			}
			h.annotateCartridgeElementAddresses(r.Context(), []*domain.VirtualCartridge{cartridge})
			respondJSON(w, http.StatusOK, cartridge)
		case http.MethodDelete:
			deleteCartridge("self-asserted:web-console")
		default:
			respondError(w, http.StatusMethodNotAllowed, "method not allowed", nil)
		}
		return
	}

	if len(parts) == 2 && parts[1] == "delete" {
		if r.Method != http.MethodPost {
			respondError(w, http.StatusMethodNotAllowed, "method not allowed", nil)
			return
		}
		var req resourceActorRequest
		if err := decodeOptionalJSONBody(r, &req); err != nil {
			respondResourceError(w, err)
			return
		}
		actor, err := selfAssertedAuditActor(req.Actor)
		if err != nil {
			respondResourceError(w, err)
			return
		}
		deleteCartridge(actor)
		return
	}

	if len(parts) == 2 && parts[1] == "erase" {
		if r.Method != http.MethodPost {
			respondError(w, http.StatusMethodNotAllowed, "method not allowed", nil)
			return
		}
		var req eraseCartridgeRequest
		if err := decodeRequiredJSONBody(r, &req); err != nil {
			respondResourceError(w, err)
			return
		}
		actor, err := selfAssertedAuditActor(req.Actor)
		if err != nil {
			respondResourceError(w, err)
			return
		}
		req.Actor = actor
		cartridge, err := h.eraseCartridge(r.Context(), cartridgeID, req)
		if err != nil {
			respondResourceError(w, err)
			return
		}
		respondJSON(w, http.StatusOK, cartridge)
		return
	}

	if len(parts) == 2 && parts[1] == "export" {
		if r.Method != http.MethodPost {
			respondError(w, http.StatusMethodNotAllowed, "method not allowed", nil)
			return
		}
		var req resourceActorRequest
		if err := decodeOptionalJSONBody(r, &req); err != nil {
			respondResourceError(w, err)
			return
		}
		actor, err := selfAssertedAuditActor(req.Actor)
		if err != nil {
			respondResourceError(w, err)
			return
		}
		cartridge, err := h.exportCartridge(r.Context(), cartridgeID, actor)
		if err != nil {
			respondResourceError(w, err)
			return
		}
		respondJSON(w, http.StatusOK, cartridge)
		return
	}

	if len(parts) == 2 && parts[1] == "import" {
		if r.Method != http.MethodPost {
			respondError(w, http.StatusMethodNotAllowed, "method not allowed", nil)
			return
		}
		var req resourceActorRequest
		if err := decodeOptionalJSONBody(r, &req); err != nil {
			respondResourceError(w, err)
			return
		}
		actor, err := selfAssertedAuditActor(req.Actor)
		if err != nil {
			respondResourceError(w, err)
			return
		}
		cartridge, err := h.importCartridge(r.Context(), cartridgeID, actor)
		if err != nil {
			respondResourceError(w, err)
			return
		}
		respondJSON(w, http.StatusOK, cartridge)
		return
	}

	respondError(w, http.StatusNotFound, "not found", nil)
}

func (h *ResourcesHandler) handleCreateChain(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "method not allowed", nil)
		return
	}
	var req resourceChainRequest
	if err := decodeOptionalJSONBody(r, &req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request", err)
		return
	}
	if req.PoolID == "" {
		req = resourceChainRequest{
			PoolID:      "pool-demo",
			PoolName:    "demo-pool",
			LibraryID:   "lib-demo",
			LibraryName: "demo-library",
			DriveID:     "drive-demo",
			DriveSlot:   1,
			CartridgeID: "car-demo",
			Barcode:     "B001",
		}
	}
	req.PoolID = strings.TrimSpace(req.PoolID)
	req.PoolName = strings.TrimSpace(req.PoolName)
	req.LibraryID = strings.TrimSpace(req.LibraryID)
	req.LibraryName = strings.TrimSpace(req.LibraryName)
	req.DriveID = strings.TrimSpace(req.DriveID)
	req.CartridgeID = strings.TrimSpace(req.CartridgeID)
	req.Barcode = strings.TrimSpace(req.Barcode)

	if h.storage == nil {
		respondResourceError(w, domain.ErrInvalidState)
		return
	}
	poolName := nonEmpty(req.PoolName, req.PoolID)
	storagePool, err := h.storage.GetPool(r.Context(), req.PoolID)
	if errors.Is(err, domain.ErrNotFound) {
		storagePool, err = h.storage.CreatePool(r.Context(), orchestration.CreateStoragePoolRequest{
			PoolID:              req.PoolID,
			Name:                poolName,
			WarningThresholdPct: 90,
			Actor:               "self-asserted:unspecified",
		})
	}
	if err != nil {
		respondResourceError(w, err)
		return
	}
	lib, err := domain.NewVirtualLibrary(req.LibraryID, nonEmpty(req.LibraryName, req.LibraryID))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid request", err)
		return
	}
	drive, err := domain.NewVirtualDrive(req.DriveID, lib.LibraryID, nonZeroInt(req.DriveSlot, 1))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid request", err)
		return
	}
	if h.wouldExceedLibraryDriveLimit(r.Context(), drive.LibraryID, drive.DriveID) {
		respondResourceError(w, domain.ErrInvalidInput)
		return
	}
	cart := domain.NewVirtualCartridge(req.CartridgeID, req.PoolID, lib.LibraryID, nonEmpty(req.Barcode, "B001"), 1<<30)
	cart.UpdatedAt = time.Now().UTC()

	ctx := r.Context()
	if err := h.repo.SaveLibrary(ctx, lib); err != nil {
		respondResourceError(w, err)
		return
	}
	if err := h.repo.SaveDrive(ctx, drive); err != nil {
		respondResourceError(w, err)
		return
	}
	if err := h.repo.SaveCartridge(ctx, cart); err != nil {
		respondResourceError(w, err)
		return
	}
	if err := h.syncLibrarySlotsToSharedState(ctx, lib.LibraryID); err != nil {
		respondResourceError(w, err)
		return
	}
	if err := h.ensureLibraryAutoPublications(ctx, lib.LibraryID); err != nil {
		respondResourceError(w, err)
		return
	}

	respondJSON(w, http.StatusCreated, resourceChainResponse{
		Pool:      legacyPoolFromStoragePool(storagePool),
		Library:   lib,
		Drive:     drive,
		Cartridge: cart,
	})
}

func resourceIDFromPath(path, prefix string) string {
	id := strings.Trim(strings.TrimPrefix(path, prefix), "/")
	if id == "" || strings.Contains(id, "/") {
		return ""
	}
	return id
}

func (h *ResourcesHandler) eraseCartridge(ctx context.Context, cartridgeID string, req eraseCartridgeRequest) (*domain.VirtualCartridge, error) {
	mode := strings.ToLower(strings.TrimSpace(req.Mode))
	if mode != "short" && mode != "long" {
		return nil, domain.ErrInvalidInput
	}
	if err := h.reconcileMediaState(ctx); err != nil {
		return nil, err
	}
	cartridge, err := h.repo.FindCartridge(ctx, cartridgeID)
	if err != nil {
		return nil, err
	}
	actor := nonEmpty(strings.TrimSpace(req.Actor), "web-console")
	for _, drive := range h.repo.ListDrives(ctx) {
		if drive != nil && strings.TrimSpace(drive.MountedCartridgeID) == cartridge.CartridgeID {
			h.emitCartridgeAudit(ctx, actor, "cartridge_erase", cartridge, "failure", map[string]any{"mode": mode, "reason": "mounted"})
			return nil, domain.ErrConflict
		}
	}
	if cartridge.RetentionState == domain.RetentionLocked {
		h.emitCartridgeAudit(ctx, actor, "cartridge_erase", cartridge, "failure", map[string]any{"mode": mode, "reason": "retention_locked"})
		return nil, domain.ErrConflict
	}
	if err := h.unpublishDependentPublications(ctx, "", "", cartridge.CartridgeID); err != nil {
		h.emitCartridgeAudit(ctx, actor, "cartridge_erase", cartridge, "failure", map[string]any{"mode": mode, "reason": "unpublish"})
		return nil, err
	}
	var cleanupErr error
	if mode == "long" {
		cleanupErr = removeCartridgeLayoutArtifacts(cartridge)
	} else {
		cleanupErr = resetCartridgeLayoutArtifacts(cartridge)
	}
	if cleanupErr != nil {
		h.emitCartridgeAudit(ctx, actor, "cartridge_erase", cartridge, "failure", map[string]any{"mode": mode, "reason": "remove_artifacts"})
		return nil, cleanupErr
	}
	cartridge.UsedBytes = 0
	cartridge.UpdatedAt = time.Now().UTC()
	if err := removeCartridgeMetadataFile(cartridge); err != nil {
		h.emitCartridgeAudit(ctx, actor, "cartridge_erase", cartridge, "failure", map[string]any{"mode": mode, "reason": "remove_metadata"})
		return nil, err
	}
	if err := writeCartridgeMetadata(cartridge); err != nil {
		h.emitCartridgeAudit(ctx, actor, "cartridge_erase", cartridge, "failure", map[string]any{"mode": mode, "reason": "write_metadata"})
		return nil, err
	}
	if err := h.repo.SaveCartridge(ctx, cartridge); err != nil {
		h.emitCartridgeAudit(ctx, actor, "cartridge_erase", cartridge, "failure", map[string]any{"mode": mode, "reason": "save_cartridge"})
		return nil, err
	}
	if err := h.syncPoolUsage(ctx, cartridge.PoolID); err != nil {
		h.emitCartridgeAudit(ctx, actor, "cartridge_erase", cartridge, "failure", map[string]any{"mode": mode, "reason": "sync_pool_usage"})
		return nil, err
	}
	h.emitCartridgeAudit(ctx, actor, "cartridge_erase", cartridge, "success", map[string]any{"mode": mode})
	return cartridge, nil
}

func (h *ResourcesHandler) emitCartridgeAudit(ctx context.Context, actor, action string, cartridge *domain.VirtualCartridge, result string, details map[string]any) {
	if h.auditW == nil || cartridge == nil {
		return
	}
	if details == nil {
		details = make(map[string]any)
	}
	details["barcode"] = cartridge.Barcode
	details["poolId"] = cartridge.PoolID
	details["libraryId"] = cartridge.LibraryID
	evt := audit.Event{
		EventID:    fmt.Sprintf("%s-%s-%d", action, cartridge.CartridgeID, time.Now().UTC().UnixNano()),
		Actor:      audit.NormalizeServiceActor(actor),
		Action:     action,
		ObjectType: "cartridge",
		ObjectID:   cartridge.CartridgeID,
		Result:     result,
		Details:    details,
		OccurredAt: time.Now().UTC(),
	}
	if err := h.auditW.Write(ctx, evt); err != nil {
		log.Printf("AUDIT WRITE FAILURE: %v (event: %s/%s)", err, evt.Action, evt.ObjectID)
	}
}

func (h *ResourcesHandler) emitLibraryAudit(ctx context.Context, actor, action string, library *domain.VirtualLibrary, result string, details map[string]any) {
	if h.auditW == nil || library == nil {
		return
	}
	if details == nil {
		details = make(map[string]any)
	}
	details["libraryId"] = library.LibraryID
	evt := audit.Event{
		EventID:    fmt.Sprintf("%s-%s-%d", action, library.LibraryID, time.Now().UTC().UnixNano()),
		Actor:      audit.NormalizeServiceActor(actor),
		Action:     action,
		ObjectType: "library",
		ObjectID:   library.LibraryID,
		Result:     result,
		Details:    details,
		OccurredAt: time.Now().UTC(),
	}
	if err := h.auditW.Write(ctx, evt); err != nil {
		log.Printf("AUDIT WRITE FAILURE: %v (event: %s/%s)", err, evt.Action, evt.ObjectID)
	}
}

func respondResourceError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	message := "internal server error"
	switch {
	case errors.Is(err, domain.ErrInvalidInput), errors.Is(err, domain.ErrInvalidState):
		status = http.StatusBadRequest
		message = "invalid request"
	case errors.Is(err, domain.ErrNotFound):
		status = http.StatusNotFound
		message = "resource not found"
	case errors.Is(err, domain.ErrConflict):
		status = http.StatusConflict
		message = "resource conflict"
	}
	respondError(w, status, message, err)
}

func nonEmpty(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func nonZero(value, fallback int64) int64 {
	if value <= 0 {
		return fallback
	}
	return value
}

func nonZeroInt(value, fallback int) int {
	if value <= 0 {
		return fallback
	}
	return value
}

func (h *ResourcesHandler) wouldExceedLibraryDriveLimit(ctx context.Context, libraryID, driveID string) bool {
	libraryID = strings.TrimSpace(libraryID)
	driveID = strings.TrimSpace(driveID)
	count := 0
	for _, drive := range h.repo.ListDrives(ctx) {
		if drive == nil || strings.TrimSpace(drive.LibraryID) != libraryID {
			continue
		}
		if driveID != "" && strings.TrimSpace(drive.DriveID) == driveID {
			continue
		}
		count++
	}
	return count >= maxLibraryDriveCount
}

func (h *ResourcesHandler) addLibrarySlots(ctx context.Context, libraryID string, count int, actor string) (*domain.VirtualLibrary, error) {
	libraryID = strings.TrimSpace(libraryID)
	if validateManagementID(libraryID) != nil {
		return nil, domain.ErrInvalidInput
	}
	if _, err := h.repo.FindLibrary(ctx, libraryID); err != nil {
		return nil, err
	}
	unlock := h.lockLibrarySlots(libraryID)
	defer unlock()
	library, added, err := h.addLibrarySlotsLocked(ctx, libraryID, count)
	if err != nil {
		return nil, err
	}
	h.emitLibraryAudit(ctx, actor, "library_add_slots", library, "success", map[string]any{
		"addedSlots": added,
		"slotCount":  library.SlotCount,
	})
	return library, nil
}

func (h *ResourcesHandler) addLibrarySlotsLocked(ctx context.Context, libraryID string, count int) (*domain.VirtualLibrary, int, error) {
	library, err := h.repo.FindLibrary(ctx, strings.TrimSpace(libraryID))
	if err != nil {
		return nil, 0, err
	}
	if count < 0 || library.SlotCount < 0 {
		return nil, 0, domain.ErrInvalidInput
	}
	if count == 0 {
		count = 1
	}
	if library.SlotCount > maxLibrarySlotCount || count > maxLibrarySlotCount-library.SlotCount {
		return nil, 0, domain.ErrInvalidInput
	}
	library.SlotCount += count
	library.UpdatedAt = time.Now().UTC()
	if err := h.repo.SaveLibrary(ctx, library); err != nil {
		return nil, 0, err
	}
	if err := h.syncLibrarySlotsToSharedState(ctx, library.LibraryID); err != nil {
		return nil, 0, err
	}
	return library, count, nil
}

func (h *ResourcesHandler) rollbackLibrarySlotExpansion(ctx context.Context, libraryID string, originalSlotCount int) error {
	if originalSlotCount < 0 {
		return domain.ErrInvalidInput
	}
	library, err := h.repo.FindLibrary(ctx, strings.TrimSpace(libraryID))
	if err != nil {
		return err
	}
	if library.SlotCount <= originalSlotCount {
		return nil
	}
	slotStart := librarySlotStart(library)
	slotEnd := slotStart + originalSlotCount
	for _, cartridge := range h.repo.ListCartridges(ctx) {
		if cartridge == nil || strings.TrimSpace(cartridge.LibraryID) != strings.TrimSpace(libraryID) {
			continue
		}
		if cartridge.LifecycleState == domain.CartridgeExported || cartridge.LifecycleState == domain.CartridgeRetired {
			continue
		}
		if cartridge.AssignedSlotAddress != nil && *cartridge.AssignedSlotAddress >= slotEnd {
			return domain.ErrConflict
		}
	}
	library.SlotCount = originalSlotCount
	library.UpdatedAt = time.Now().UTC()
	if err := h.repo.SaveLibrary(ctx, library); err != nil {
		return err
	}
	return h.syncLibrarySlotsToSharedState(ctx, library.LibraryID)
}

func (h *ResourcesHandler) assignSlotForNewCartridge(ctx context.Context, library *domain.VirtualLibrary, expandSlots bool) (int, bool, error) {
	if library == nil {
		return 0, false, domain.ErrInvalidInput
	}
	cartridges := h.repo.ListCartridges(ctx)
	labelSlots := h.libraryLabelSlots(ctx, library)
	if slot, ok := h.nextEmptySlotAddressFromSnapshot(library, cartridges, labelSlots); ok {
		return slot, false, nil
	}
	if !expandSlots {
		return 0, false, domain.ErrConflict
	}
	updated, _, err := h.addLibrarySlotsLocked(ctx, library.LibraryID, 1)
	if err != nil {
		return 0, false, err
	}
	cartridges = h.repo.ListCartridges(ctx)
	labelSlots = h.libraryLabelSlots(ctx, updated)
	if slot, ok := h.nextEmptySlotAddressFromSnapshot(updated, cartridges, labelSlots); ok {
		return slot, true, nil
	}
	return 0, true, domain.ErrConflict
}

func (h *ResourcesHandler) nextEmptySlotAddress(ctx context.Context, library *domain.VirtualLibrary) (int, bool) {
	if library == nil {
		return 0, false
	}
	return h.nextEmptySlotAddressFromSnapshot(library, h.repo.ListCartridges(ctx), h.libraryLabelSlots(ctx, library))
}

func (h *ResourcesHandler) nextEmptySlotAddressFromSnapshot(library *domain.VirtualLibrary, cartridges []*domain.VirtualCartridge, labelSlots map[string]int) (int, bool) {
	if library == nil {
		return 0, false
	}
	slotCount := library.SlotCount
	if slotCount <= 0 {
		return 0, false
	}
	slotStart := librarySlotStart(library)
	occupied := occupiedSlotAddressesFromSnapshot(library.LibraryID, cartridges, labelSlots)
	for idx := 0; idx < slotCount; idx++ {
		address := slotStart + idx
		if _, exists := occupied[address]; !exists {
			return address, true
		}
	}
	return 0, false
}

func (h *ResourcesHandler) occupiedSlotAddresses(ctx context.Context, libraryID string) map[int]struct{} {
	library, err := h.repo.FindLibrary(ctx, libraryID)
	if err != nil {
		return make(map[int]struct{})
	}
	return occupiedSlotAddressesFromSnapshot(libraryID, h.repo.ListCartridges(ctx), h.libraryLabelSlots(ctx, library))
}

func (h *ResourcesHandler) libraryLabelSlots(ctx context.Context, library *domain.VirtualLibrary) map[string]int {
	labelSlots := make(map[string]int)
	if library == nil {
		return labelSlots
	}
	slotStart := librarySlotStart(library)
	for _, label := range readLibrarySlotLabels(ctx, h.repo, library.LibraryID) {
		key := strings.ToUpper(strings.TrimSpace(label.label))
		if key != "" {
			labelSlots[key] = slotStart + label.index
		}
	}
	return labelSlots
}

func occupiedSlotAddressesFromSnapshot(libraryID string, cartridges []*domain.VirtualCartridge, labelSlots map[string]int) map[int]struct{} {
	occupied := make(map[int]struct{})
	for _, cartridge := range cartridges {
		if cartridge == nil || strings.TrimSpace(cartridge.LibraryID) != libraryID {
			continue
		}
		if cartridge.LifecycleState == domain.CartridgeExported || cartridge.LifecycleState == domain.CartridgeRetired {
			continue
		}
		if cartridge.AssignedSlotAddress != nil {
			occupied[*cartridge.AssignedSlotAddress] = struct{}{}
			continue
		}
		for _, raw := range []string{cartridge.CartridgeID, cartridge.Barcode} {
			if address, ok := labelSlots[strings.ToUpper(strings.TrimSpace(raw))]; ok {
				occupied[address] = struct{}{}
				break
			}
		}
	}
	return occupied
}

func (h *ResourcesHandler) currentOrAssignedSlotAddress(ctx context.Context, cartridge *domain.VirtualCartridge) (int, bool) {
	if cartridge == nil {
		return 0, false
	}
	if cartridge.AssignedSlotAddress != nil {
		return *cartridge.AssignedSlotAddress, true
	}
	return h.currentSlotAddress(ctx, cartridge)
}

func (h *ResourcesHandler) currentSlotAddress(ctx context.Context, cartridge *domain.VirtualCartridge) (int, bool) {
	if cartridge == nil {
		return 0, false
	}
	library, err := h.repo.FindLibrary(ctx, cartridge.LibraryID)
	if err != nil {
		return 0, false
	}
	slotStart := librarySlotStart(library)
	for _, label := range readLibrarySlotLabels(ctx, h.repo, cartridge.LibraryID) {
		key := strings.ToUpper(strings.TrimSpace(label.label))
		for _, raw := range []string{cartridge.CartridgeID, cartridge.Barcode} {
			if key != "" && key == strings.ToUpper(strings.TrimSpace(raw)) {
				return slotStart + label.index, true
			}
		}
	}
	return 0, false
}

func (h *ResourcesHandler) assignedSlotAvailableFor(ctx context.Context, libraryID string, assignedSlot *int, cartridgeID string) bool {
	if assignedSlot == nil {
		return false
	}
	library, err := h.repo.FindLibrary(ctx, libraryID)
	if err != nil {
		return false
	}
	slotStart := librarySlotStart(library)
	slotEnd := slotStart + library.SlotCount
	if *assignedSlot < slotStart || *assignedSlot >= slotEnd {
		return false
	}
	for _, cartridge := range h.repo.ListCartridges(ctx) {
		if cartridge == nil || strings.TrimSpace(cartridge.LibraryID) != libraryID || cartridge.CartridgeID == cartridgeID {
			continue
		}
		if cartridge.LifecycleState == domain.CartridgeExported || cartridge.LifecycleState == domain.CartridgeRetired {
			continue
		}
		if cartridge.AssignedSlotAddress != nil && *cartridge.AssignedSlotAddress == *assignedSlot {
			return false
		}
	}
	for _, label := range readLibrarySlotLabels(ctx, h.repo, libraryID) {
		if slotStart+label.index != *assignedSlot {
			continue
		}
		key := strings.ToUpper(strings.TrimSpace(label.label))
		if key == "" {
			continue
		}
		for _, cartridge := range h.repo.ListCartridges(ctx) {
			if cartridge == nil || strings.TrimSpace(cartridge.LibraryID) != libraryID || cartridge.CartridgeID == cartridgeID {
				continue
			}
			for _, raw := range []string{cartridge.CartridgeID, cartridge.Barcode} {
				if key == strings.ToUpper(strings.TrimSpace(raw)) {
					return false
				}
			}
		}
	}
	return true
}

func librarySlotStart(library *domain.VirtualLibrary) int {
	if library == nil || library.SlotStartAddress <= 0 {
		return 1
	}
	return library.SlotStartAddress
}

func legacyPoolFromStoragePool(pool *domain.StoragePoolRuntime) *domain.StoragePool {
	if pool == nil {
		return nil
	}
	return &domain.StoragePool{
		Timestamped:  pool.Timestamped,
		PoolID:       pool.PoolID,
		Name:         pool.Name,
		CapacityByte: pool.Capacity.TotalBytes,
		UsedByte:     pool.Capacity.UsedBytes,
		Status:       domain.PoolStatus(pool.Status),
	}
}

func (h *ResourcesHandler) loadCartridgeIntoDrive(ctx context.Context, driveID, cartridgeID, _ string) (*domain.VirtualDrive, error) {
	driveID = strings.TrimSpace(driveID)
	cartridgeID = strings.TrimSpace(cartridgeID)
	if driveID == "" || cartridgeID == "" {
		return nil, domain.ErrInvalidInput
	}
	drive, err := h.repo.FindDrive(ctx, driveID)
	if err != nil {
		return nil, err
	}
	unlock := h.lockLibrarySlots(drive.LibraryID)
	defer unlock()
	if err := h.reconcileMediaState(ctx); err != nil {
		return nil, err
	}
	drive, err = h.repo.FindDrive(ctx, driveID)
	if err != nil {
		return nil, err
	}
	cartridge, err := h.repo.FindCartridge(ctx, cartridgeID)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(drive.LibraryID) != strings.TrimSpace(cartridge.LibraryID) {
		return nil, domain.ErrInvalidState
	}
	if drive.MountState != domain.MountEmpty || strings.TrimSpace(drive.MountedCartridgeID) != "" {
		return nil, domain.ErrInvalidState
	}
	if cartridge.LifecycleState != domain.CartridgeAvailable {
		return nil, domain.ErrInvalidState
	}
	assignedSlot, ok := h.currentOrAssignedSlotAddress(ctx, cartridge)
	if !ok {
		return nil, domain.ErrConflict
	}
	cartridge.AssignedSlotAddress = &assignedSlot

	if err := drive.Mount(cartridge.CartridgeID); err != nil {
		return nil, err
	}
	if err := cartridge.TransitionTo(domain.CartridgeMounted); err != nil {
		return nil, err
	}
	if err := h.repo.SaveCartridge(ctx, cartridge); err != nil {
		return nil, err
	}
	if err := h.repo.SaveDrive(ctx, drive); err != nil {
		return nil, err
	}
	if err := writeDriveMediaState(drive.LibraryID, drive.DriveID, cartridge.CartridgeID); err != nil {
		return nil, err
	}
	if err := h.syncLibrarySlotsToSharedState(ctx, drive.LibraryID); err != nil {
		return nil, err
	}
	return drive, nil
}

func (h *ResourcesHandler) unloadDrive(ctx context.Context, driveID, _ string) (*domain.VirtualDrive, error) {
	driveID = strings.TrimSpace(driveID)
	if driveID == "" {
		return nil, domain.ErrInvalidInput
	}
	drive, err := h.repo.FindDrive(ctx, driveID)
	if err != nil {
		return nil, err
	}
	unlock := h.lockLibrarySlots(drive.LibraryID)
	defer unlock()
	if err := h.reconcileMediaState(ctx); err != nil {
		return nil, err
	}
	drive, err = h.repo.FindDrive(ctx, driveID)
	if err != nil {
		return nil, err
	}
	mountedCartridgeID := strings.TrimSpace(drive.MountedCartridgeID)
	if drive.MountState != domain.MountLoaded || mountedCartridgeID == "" {
		return nil, domain.ErrInvalidState
	}
	cartridge, err := h.repo.FindCartridge(ctx, mountedCartridgeID)
	if err != nil {
		return nil, err
	}
	if cartridge.LifecycleState != domain.CartridgeMounted {
		return nil, domain.ErrInvalidState
	}
	if cartridge.AssignedSlotAddress == nil {
		library, err := h.repo.FindLibrary(ctx, cartridge.LibraryID)
		if err != nil {
			return nil, err
		}
		slotAddress, ok := h.nextEmptySlotAddress(ctx, library)
		if !ok {
			return nil, domain.ErrConflict
		}
		cartridge.AssignedSlotAddress = &slotAddress
	}
	if !h.assignedSlotAvailableFor(ctx, cartridge.LibraryID, cartridge.AssignedSlotAddress, cartridge.CartridgeID) {
		return nil, domain.ErrConflict
	}

	if err := drive.Unmount(); err != nil {
		return nil, err
	}
	if err := cartridge.TransitionTo(domain.CartridgeAvailable); err != nil {
		return nil, err
	}
	cartridge.CurrentElementAddress = cartridge.AssignedSlotAddress
	if err := h.repo.SaveCartridge(ctx, cartridge); err != nil {
		return nil, err
	}
	if err := h.repo.SaveDrive(ctx, drive); err != nil {
		return nil, err
	}
	if err := writeDriveMediaState(drive.LibraryID, drive.DriveID, ""); err != nil {
		return nil, err
	}
	if err := h.syncLibrarySlotsToSharedState(ctx, drive.LibraryID); err != nil {
		return nil, err
	}
	return drive, nil
}

func (h *ResourcesHandler) exportCartridge(ctx context.Context, cartridgeID, _ string) (*domain.VirtualCartridge, error) {
	cartridgeID = strings.TrimSpace(cartridgeID)
	if cartridgeID == "" {
		return nil, domain.ErrInvalidInput
	}
	cartridge, err := h.repo.FindCartridge(ctx, cartridgeID)
	if err != nil {
		return nil, err
	}
	unlock := h.lockLibrarySlots(cartridge.LibraryID)
	defer unlock()
	if err := h.reconcileMediaState(ctx); err != nil {
		return nil, err
	}
	cartridge, err = h.repo.FindCartridge(ctx, cartridgeID)
	if err != nil {
		return nil, err
	}
	if cartridge.LifecycleState != domain.CartridgeAvailable {
		return nil, domain.ErrInvalidState
	}
	currentSlot, hasCurrentSlot := h.currentSlotAddress(ctx, cartridge)
	if !hasCurrentSlot {
		return nil, domain.ErrConflict
	}
	if cartridge.AssignedSlotAddress == nil || *cartridge.AssignedSlotAddress != currentSlot {
		cartridge.AssignedSlotAddress = &currentSlot
	}
	if err := cartridge.TransitionTo(domain.CartridgeExported); err != nil {
		return nil, err
	}
	if err := h.repo.SaveCartridge(ctx, cartridge); err != nil {
		return nil, err
	}
	if err := h.syncLibrarySlotsToSharedState(ctx, cartridge.LibraryID); err != nil {
		return nil, err
	}
	return cartridge, nil
}

func (h *ResourcesHandler) importCartridge(ctx context.Context, cartridgeID, actor string) (*domain.VirtualCartridge, error) {
	cartridgeID = strings.TrimSpace(cartridgeID)
	if cartridgeID == "" {
		return nil, domain.ErrInvalidInput
	}
	cartridge, err := h.repo.FindCartridge(ctx, cartridgeID)
	if err != nil {
		return nil, err
	}
	unlock := h.lockLibrarySlots(cartridge.LibraryID)
	defer unlock()
	if err := h.reconcileMediaState(ctx); err != nil {
		return nil, err
	}
	cartridge, err = h.repo.FindCartridge(ctx, cartridgeID)
	if err != nil {
		return nil, err
	}
	if cartridge.LifecycleState != domain.CartridgeExported {
		return nil, domain.ErrInvalidState
	}
	importSlot, oldSlot, reassigned, err := h.importSlotForVaultCartridge(ctx, cartridge)
	if err != nil {
		return nil, err
	}
	cartridge.AssignedSlotAddress = &importSlot
	if reassigned {
		log.Printf("vault import reassigned slot cartridge=%s library=%s oldAssignedSlot=%d newAssignedSlot=%d reason=assigned_slot_occupied_or_missing", cartridge.CartridgeID, cartridge.LibraryID, oldSlot, importSlot)
	}
	if err := cartridge.TransitionTo(domain.CartridgeImported); err != nil {
		return nil, err
	}
	if err := cartridge.TransitionTo(domain.CartridgeAvailable); err != nil {
		return nil, err
	}
	cartridge.CurrentElementAddress = &importSlot
	if err := h.repo.SaveCartridge(ctx, cartridge); err != nil {
		return nil, err
	}
	if err := h.syncLibrarySlotsToSharedState(ctx, cartridge.LibraryID); err != nil {
		return nil, err
	}
	if reassigned {
		h.emitCartridgeAudit(ctx, actor, "cartridge_slot_reassigned", cartridge, "success", map[string]any{
			"oldAssignedSlot": oldSlot,
			"newAssignedSlot": importSlot,
			"reason":          "vault_import_assigned_slot_unavailable",
		})
	}
	return cartridge, nil
}

func (h *ResourcesHandler) importSlotForVaultCartridge(ctx context.Context, cartridge *domain.VirtualCartridge) (int, int, bool, error) {
	if cartridge == nil {
		return 0, 0, false, domain.ErrInvalidInput
	}
	oldSlot := 0
	if cartridge.AssignedSlotAddress != nil {
		oldSlot = *cartridge.AssignedSlotAddress
		if h.assignedSlotAvailableFor(ctx, cartridge.LibraryID, cartridge.AssignedSlotAddress, cartridge.CartridgeID) {
			return oldSlot, oldSlot, false, nil
		}
	}
	library, err := h.repo.FindLibrary(ctx, cartridge.LibraryID)
	if err != nil {
		return 0, oldSlot, false, err
	}
	nextSlot, ok := h.nextEmptySlotAddress(ctx, library)
	if !ok {
		return 0, oldSlot, false, domain.ErrConflict
	}
	return nextSlot, oldSlot, true, nil
}
