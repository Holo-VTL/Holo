package api

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Holo-VTL/Holo/control-plane/internal/audit"
	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
	"github.com/Holo-VTL/Holo/control-plane/internal/orchestration"
	"github.com/Holo-VTL/Holo/control-plane/internal/storageutil"
	"github.com/Holo-VTL/Holo/control-plane/internal/tracing"
)

type ResourcesHandler struct {
	repo    coreResourcesRepo
	storage resourceStoragePoolService
	target  resourceTargetService
	auditW  audit.Writer
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

func NewResourcesHandler(repo coreResourcesRepo, storage resourceStoragePoolService, target resourceTargetService) *ResourcesHandler {
	return &ResourcesHandler{repo: repo, storage: storage, target: target}
}

func NewResourcesHandlerWithAudit(repo coreResourcesRepo, storage resourceStoragePoolService, target resourceTargetService, auditW audit.Writer) *ResourcesHandler {
	return &ResourcesHandler{repo: repo, storage: storage, target: target, auditW: auditW}
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
	CapacityBytes int64  `json:"capacityBytes"`
	LTOGeneration int    `json:"ltoGeneration,omitempty"`
	MediaType     string `json:"mediaType,omitempty"`
}

type loadDriveRequest struct {
	CartridgeID string `json:"cartridgeId"`
	Actor       string `json:"actor,omitempty"`
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
		drive, err := h.loadCartridgeIntoDrive(r.Context(), driveID, req.CartridgeID, req.Actor)
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
		drive, err := h.unloadDrive(r.Context(), driveID, req.Actor)
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
		if _, err := h.repo.FindLibrary(r.Context(), req.LibraryID); err != nil {
			respondResourceError(w, err)
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
		cartridgeID, barcode, err := h.resolveCartridgeIdentity(r.Context(), req)
		if err != nil {
			respondResourceError(w, err)
			return
		}
		cartridge := domain.NewVirtualCartridge(cartridgeID, req.PoolID, req.LibraryID, barcode, req.CapacityBytes)
		cartridge.UpdatedAt = time.Now().UTC()
		if err := h.repo.CreateCartridge(r.Context(), cartridge); err != nil {
			respondResourceError(w, err)
			return
		}
		if err := writeCartridgeMetadata(cartridge); err != nil {
			h.logCompensationError(r.Context(), "delete cartridge after metadata failure", h.repo.DeleteCartridge(r.Context(), cartridge.CartridgeID), "cartridgeId", cartridge.CartridgeID, "libraryId", cartridge.LibraryID)
			respondResourceError(w, err)
			return
		}
		if err := h.syncLibrarySlotsToSharedState(r.Context(), cartridge.LibraryID); err != nil {
			h.logCompensationError(r.Context(), "delete cartridge after slot sync failure", h.repo.DeleteCartridge(r.Context(), cartridge.CartridgeID), "cartridgeId", cartridge.CartridgeID, "libraryId", cartridge.LibraryID)
			respondResourceError(w, err)
			return
		}
		if err := h.syncPoolUsage(r.Context(), cartridge.PoolID); err != nil {
			h.logCompensationError(r.Context(), "delete cartridge after pool usage sync failure", h.repo.DeleteCartridge(r.Context(), cartridge.CartridgeID), "poolId", cartridge.PoolID, "cartridgeId", cartridge.CartridgeID)
			h.logCompensationError(r.Context(), "resync library slots after pool usage sync failure", h.syncLibrarySlotsToSharedState(r.Context(), cartridge.LibraryID), "libraryId", cartridge.LibraryID)
			respondResourceError(w, err)
			return
		}
		if err := h.ensureLibraryAutoPublications(r.Context(), cartridge.LibraryID); err != nil {
			h.logCompensationError(r.Context(), "delete cartridge after publication failure", h.repo.DeleteCartridge(r.Context(), cartridge.CartridgeID), "cartridgeId", cartridge.CartridgeID, "libraryId", cartridge.LibraryID)
			h.logCompensationError(r.Context(), "resync library slots after publication failure", h.syncLibrarySlotsToSharedState(r.Context(), cartridge.LibraryID), "libraryId", cartridge.LibraryID)
			respondResourceError(w, err)
			return
		}
		respondJSON(w, http.StatusCreated, cartridge)
	case http.MethodGet:
		if err := h.reconcileMediaState(r.Context()); err != nil {
			respondResourceError(w, err)
			return
		}
		respondJSON(w, http.StatusOK, h.repo.ListCartridges(r.Context()))
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
			respondJSON(w, http.StatusOK, cartridge)
		case http.MethodDelete:
			deleteCartridge("web-console")
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
		deleteCartridge(req.Actor)
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
		cartridge, err := h.exportCartridge(r.Context(), cartridgeID, req.Actor)
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
		cartridge, err := h.importCartridge(r.Context(), cartridgeID, req.Actor)
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
			Actor:               "system",
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
		Actor:      nonEmpty(strings.TrimSpace(actor), "web-console"),
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
	if status == http.StatusInternalServerError {
		respondError(w, status, message, err)
		return
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
	if err := h.reconcileMediaState(ctx); err != nil {
		return nil, err
	}

	drive, err := h.repo.FindDrive(ctx, driveID)
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
	if err := h.reconcileMediaState(ctx); err != nil {
		return nil, err
	}

	drive, err := h.repo.FindDrive(ctx, driveID)
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

	if err := drive.Unmount(); err != nil {
		return nil, err
	}
	if err := cartridge.TransitionTo(domain.CartridgeAvailable); err != nil {
		return nil, err
	}
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
	if err := h.reconcileMediaState(ctx); err != nil {
		return nil, err
	}

	cartridge, err := h.repo.FindCartridge(ctx, cartridgeID)
	if err != nil {
		return nil, err
	}
	if cartridge.LifecycleState != domain.CartridgeAvailable {
		return nil, domain.ErrInvalidState
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

func (h *ResourcesHandler) importCartridge(ctx context.Context, cartridgeID, _ string) (*domain.VirtualCartridge, error) {
	cartridgeID = strings.TrimSpace(cartridgeID)
	if cartridgeID == "" {
		return nil, domain.ErrInvalidInput
	}
	if err := h.reconcileMediaState(ctx); err != nil {
		return nil, err
	}

	cartridge, err := h.repo.FindCartridge(ctx, cartridgeID)
	if err != nil {
		return nil, err
	}
	if cartridge.LifecycleState != domain.CartridgeExported {
		return nil, domain.ErrInvalidState
	}
	if err := cartridge.TransitionTo(domain.CartridgeImported); err != nil {
		return nil, err
	}
	if err := cartridge.TransitionTo(domain.CartridgeAvailable); err != nil {
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

func (h *ResourcesHandler) syncLibrarySlotsToSharedState(ctx context.Context, libraryID string) error {
	if strings.TrimSpace(libraryID) == "" {
		return domain.ErrInvalidInput
	}

	library, err := h.repo.FindLibrary(ctx, libraryID)
	if err != nil {
		return err
	}

	cartridges := h.repo.ListCartridges(ctx)
	filtered := make([]*domain.VirtualCartridge, 0)
	exported := make([]*domain.VirtualCartridge, 0)
	activeLabels := make(map[string]struct{})
	for _, cartridge := range cartridges {
		if cartridge == nil || cartridge.LibraryID != libraryID {
			continue
		}
		if cartridge.LifecycleState == domain.CartridgeExported {
			exported = append(exported, cartridge)
			continue
		}
		if cartridge.LifecycleState != domain.CartridgeMounted {
			filtered = append(filtered, cartridge)
			activeLabels[cartridgeSharedLabel(cartridge)] = struct{}{}
		}
	}
	sort.Slice(filtered, func(i, j int) bool {
		return filtered[i].CartridgeID < filtered[j].CartridgeID
	})
	sort.Slice(exported, func(i, j int) bool {
		if !exported[i].UpdatedAt.Equal(exported[j].UpdatedAt) {
			return exported[i].UpdatedAt.After(exported[j].UpdatedAt)
		}
		return exported[i].CartridgeID < exported[j].CartridgeID
	})
	slotCount := library.SlotCount
	if slotCount < len(filtered) {
		slotCount = len(filtered)
	}
	if slotCount <= 0 {
		slotCount = 1
	}

	drives := h.repo.ListDrives(ctx)
	driveIDs := make([]string, 0)
	for _, drive := range drives {
		if drive != nil && drive.LibraryID == libraryID {
			driveIDs = append(driveIDs, drive.DriveID)
		}
	}
	if len(driveIDs) == 0 {
		return nil
	}

	dir := mediaStateDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	existingLabels := readExistingSlotLabels(libraryID, driveIDs[0])
	if slotCount < len(existingLabels) {
		slotCount = len(existingLabels)
	}
	labels := stableSlotLabels(slotCount, existingLabels, filtered, activeLabels)
	removeLoadedLabelsFromSlots(labels, readLoadedLabels(libraryID, driveIDs))
	slotPayload := strings.Join(labels, "\n") + "\n"
	ieCount := library.IEPortCount
	if ieCount <= 0 {
		ieCount = 1
	}
	ieLabels := exportedIELabels(ieCount, nil)
	iePayload := strings.Join(ieLabels, "\n") + "\n"
	vaultLabels := exportedVaultLabels(exported)
	vaultPayload := ""
	if len(vaultLabels) > 0 {
		vaultPayload = strings.Join(vaultLabels, "\n") + "\n"
	}
	for _, driveID := range driveIDs {
		stateKey := storageutil.MediaStateKey(libraryID, driveID)
		basePath := filepath.Join(dir, sanitizeStateID(stateKey))
		if err := writeAtomicText(basePath+".slots", slotPayload); err != nil {
			return err
		}
		if err := writeAtomicText(basePath+".ie", iePayload); err != nil {
			return err
		}
		if err := writeAtomicText(basePath+".vault", vaultPayload); err != nil {
			return err
		}
	}
	for _, cartridge := range cartridges {
		if cartridge == nil || cartridge.LibraryID != libraryID {
			continue
		}
		if err := writeCartridgeMetadata(cartridge); err != nil {
			return err
		}
	}
	return nil
}

func (h *ResourcesHandler) reconcileMediaState(ctx context.Context) error {
	drives := h.repo.ListDrives(ctx)
	cartridges := h.repo.ListCartridges(ctx)
	cartridgeByLabel := make(map[string]*domain.VirtualCartridge)
	for _, cartridge := range cartridges {
		if cartridge == nil {
			continue
		}
		cartridgeByLabel[strings.ToUpper(strings.TrimSpace(cartridge.CartridgeID))] = cartridge
		cartridgeByLabel[strings.ToUpper(strings.TrimSpace(cartridge.Barcode))] = cartridge
	}

	mounted := make(map[string]struct{})
	exported := make(map[string]struct{})
	for _, drive := range drives {
		if drive == nil {
			continue
		}
		label, err := readDriveMediaState(drive.LibraryID, drive.DriveID)
		if err != nil {
			return err
		}
		mountedID := strings.TrimSpace(label)
		if cartridge := cartridgeByLabel[strings.ToUpper(mountedID)]; cartridge != nil {
			mountedID = cartridge.CartridgeID
		}

		changed := false
		if mountedID == "" {
			if drive.MountState != domain.MountEmpty || strings.TrimSpace(drive.MountedCartridgeID) != "" {
				drive.MountState = domain.MountEmpty
				drive.MountedCartridgeID = ""
				drive.UpdatedAt = time.Now().UTC()
				changed = true
			}
		} else {
			mounted[mountedID] = struct{}{}
			if drive.MountState != domain.MountLoaded || strings.TrimSpace(drive.MountedCartridgeID) != mountedID {
				drive.MountState = domain.MountLoaded
				drive.MountedCartridgeID = mountedID
				drive.UpdatedAt = time.Now().UTC()
				changed = true
			}
		}
		if changed {
			if err := h.repo.SaveDrive(ctx, drive); err != nil {
				return err
			}
		}

		for _, label := range readExistingIELabels(drive.LibraryID, drive.DriveID) {
			cartridge := cartridgeByLabel[strings.ToUpper(strings.TrimSpace(label))]
			if cartridge != nil {
				exported[cartridge.CartridgeID] = struct{}{}
			}
		}
		for _, label := range readExistingVaultLabels(drive.LibraryID, drive.DriveID) {
			cartridge := cartridgeByLabel[strings.ToUpper(strings.TrimSpace(label))]
			if cartridge != nil {
				exported[cartridge.CartridgeID] = struct{}{}
			}
		}
	}

	for _, cartridge := range cartridges {
		if cartridge == nil || cartridge.LifecycleState == domain.CartridgeRetired {
			continue
		}
		if meta, err := readCartridgeMetadata(cartridge); err != nil {
			return err
		} else if meta != nil {
			changed := false
			if meta.CapacityBytes > 0 && cartridge.CapacityBytes != meta.CapacityBytes {
				cartridge.CapacityBytes = meta.CapacityBytes
				changed = true
			}
			if meta.UsedBytes >= 0 && cartridge.UsedBytes != meta.UsedBytes {
				cartridge.UsedBytes = meta.UsedBytes
				changed = true
			}
			if changed {
				cartridge.UpdatedAt = time.Now().UTC()
			}
		}
		_, isMounted := mounted[cartridge.CartridgeID]
		_, isExported := exported[cartridge.CartridgeID]
		next := domain.CartridgeAvailable
		if isMounted {
			next = domain.CartridgeMounted
		} else if isExported {
			next = domain.CartridgeExported
		}
		if cartridge.LifecycleState != next {
			cartridge.LifecycleState = next
			cartridge.UpdatedAt = time.Now().UTC()
		}
		if err := h.repo.SaveCartridge(ctx, cartridge); err != nil {
			return err
		}
	}
	return h.syncPoolUsageForCartridges(ctx, h.repo.ListCartridges(ctx))
}

func (h *ResourcesHandler) ReconcileMediaState(ctx context.Context) error {
	return h.reconcileMediaState(ctx)
}

func (h *ResourcesHandler) syncPoolUsage(ctx context.Context, poolIDs ...string) error {
	return h.syncPoolUsageForCartridges(ctx, h.repo.ListCartridges(ctx), poolIDs...)
}

func (h *ResourcesHandler) syncPoolUsageForCartridges(ctx context.Context, cartridges []*domain.VirtualCartridge, poolIDs ...string) error {
	if h.storage == nil {
		return nil
	}
	usedByPool := make(map[string]int64)
	targetPools := make(map[string]struct{})
	for _, raw := range poolIDs {
		poolID := strings.TrimSpace(raw)
		if poolID != "" {
			targetPools[poolID] = struct{}{}
		}
	}
	for _, cartridge := range cartridges {
		if cartridge == nil || cartridge.LifecycleState == domain.CartridgeRetired {
			continue
		}
		poolID := strings.TrimSpace(cartridge.PoolID)
		if poolID == "" {
			continue
		}
		if cartridge.UsedBytes > 0 {
			const maxInt64 = int64(1<<63 - 1)
			if usedByPool[poolID] > maxInt64-cartridge.UsedBytes {
				usedByPool[poolID] = maxInt64
			} else {
				usedByPool[poolID] += cartridge.UsedBytes
			}
		}
		targetPools[poolID] = struct{}{}
	}
	for poolID := range targetPools {
		if err := h.storage.ReconcilePoolUsedBytes(ctx, poolID, usedByPool[poolID]); err != nil {
			return err
		}
	}
	return nil
}

func cartridgeSharedLabel(cartridge *domain.VirtualCartridge) string {
	if cartridge == nil {
		return ""
	}
	label := strings.TrimSpace(cartridge.Barcode)
	if label == "" {
		label = strings.TrimSpace(cartridge.CartridgeID)
	}
	return label
}

func readExistingSlotLabels(libraryID, driveID string) []string {
	stateKey := storageutil.MediaStateKey(libraryID, driveID)
	path := filepath.Join(mediaStateDir(), sanitizeStateID(stateKey)+".slots")
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	labels := make([]string, len(lines))
	for idx, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" && trimmed != "-" {
			labels[idx] = trimmed
		}
	}
	return labels
}

func readLoadedLabels(libraryID string, driveIDs []string) map[string]struct{} {
	loaded := make(map[string]struct{})
	for _, driveID := range driveIDs {
		stateKey := storageutil.MediaStateKey(libraryID, driveID)
		path := filepath.Join(mediaStateDir(), sanitizeStateID(stateKey)+".state")
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(raw), "\n") {
			key, value, ok := strings.Cut(line, "=")
			if !ok || strings.TrimSpace(key) != "cartridge" {
				continue
			}
			label := strings.TrimSpace(value)
			if label != "" {
				loaded[label] = struct{}{}
			}
		}
	}
	return loaded
}

func removeLoadedLabelsFromSlots(labels []string, loaded map[string]struct{}) {
	if len(loaded) == 0 {
		return
	}
	for idx, label := range labels {
		if _, ok := loaded[strings.TrimSpace(label)]; ok {
			labels[idx] = "-"
		}
	}
}

func readExistingIELabels(libraryID, driveID string) []string {
	stateKey := storageutil.MediaStateKey(libraryID, driveID)
	path := filepath.Join(mediaStateDir(), sanitizeStateID(stateKey)+".ie")
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	labels := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" && trimmed != "-" {
			labels = append(labels, trimmed)
		}
	}
	return labels
}

func readExistingVaultLabels(libraryID, driveID string) []string {
	stateKey := storageutil.MediaStateKey(libraryID, driveID)
	path := filepath.Join(mediaStateDir(), sanitizeStateID(stateKey)+".vault")
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	labels := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" && trimmed != "-" {
			labels = append(labels, trimmed)
		}
	}
	return labels
}

func stableSlotLabels(slotCount int, existing []string, cartridges []*domain.VirtualCartridge, active map[string]struct{}) []string {
	labels := make([]string, slotCount)
	placed := make(map[string]struct{})
	for idx := 0; idx < slotCount && idx < len(existing); idx++ {
		label := strings.TrimSpace(existing[idx])
		if label == "" {
			labels[idx] = "-"
			continue
		}
		if _, ok := active[label]; ok {
			labels[idx] = label
			placed[label] = struct{}{}
		} else {
			labels[idx] = "-"
		}
	}
	for idx := 0; idx < slotCount; idx++ {
		if labels[idx] == "" {
			labels[idx] = "-"
		}
	}
	nextSlot := 0
	for _, cartridge := range cartridges {
		label := cartridgeSharedLabel(cartridge)
		if label == "" {
			continue
		}
		if _, ok := placed[label]; ok {
			continue
		}
		for nextSlot < len(labels) && labels[nextSlot] != "-" {
			nextSlot++
		}
		if nextSlot >= len(labels) {
			break
		}
		labels[nextSlot] = label
		placed[label] = struct{}{}
	}
	return labels
}

func exportedIELabels(portCount int, cartridges []*domain.VirtualCartridge) []string {
	if portCount <= 0 {
		portCount = 1
	}
	labels := make([]string, portCount)
	for idx := range labels {
		labels[idx] = "-"
	}
	for idx, cartridge := range cartridges {
		if idx >= len(labels) {
			break
		}
		label := cartridgeSharedLabel(cartridge)
		if label == "" {
			continue
		}
		labels[idx] = label
	}
	return labels
}

func exportedVaultLabels(cartridges []*domain.VirtualCartridge) []string {
	labels := make([]string, 0, len(cartridges))
	for _, cartridge := range cartridges {
		label := cartridgeSharedLabel(cartridge)
		if label == "" {
			continue
		}
		labels = append(labels, label)
	}
	return labels
}

func readDriveMediaState(libraryID, driveID string) (string, error) {
	stateKey := storageutil.MediaStateKey(libraryID, driveID)
	path := filepath.Join(mediaStateDir(), sanitizeStateID(stateKey)+".state")
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	trimmed := strings.TrimSpace(string(raw))
	if strings.HasPrefix(trimmed, "cartridge=") {
		return strings.TrimSpace(strings.TrimPrefix(trimmed, "cartridge=")), nil
	}
	return trimmed, nil
}

func writeDriveMediaState(libraryID, driveID, cartridgeID string) error {
	stateKey := storageutil.MediaStateKey(libraryID, driveID)
	dir := mediaStateDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	targetPath := filepath.Join(dir, sanitizeStateID(stateKey)+".state")
	tmpPath := targetPath + ".tmp"
	payload := "cartridge=" + strings.TrimSpace(cartridgeID) + "\n"
	if err := os.WriteFile(tmpPath, []byte(payload), 0o644); err != nil {
		return err
	}
	return os.Rename(tmpPath, targetPath)
}

type cartridgeMetadata struct {
	CapacityBytes int64
	UsedBytes     int64
}

func cartridgeMetadataLabels(cartridge *domain.VirtualCartridge) []string {
	if cartridge == nil {
		return nil
	}
	seen := make(map[string]struct{})
	labels := make([]string, 0, 2)
	for _, raw := range []string{cartridge.CartridgeID, cartridge.Barcode} {
		label := strings.TrimSpace(raw)
		if label == "" {
			continue
		}
		key := strings.ToUpper(label)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		labels = append(labels, label)
	}
	return labels
}

func cartridgeMetadataPath(label string) string {
	return filepath.Join(mediaStateDir(), "cartridge_"+sanitizeStateID(label)+".meta")
}

func readCartridgeMetadata(cartridge *domain.VirtualCartridge) (*cartridgeMetadata, error) {
	for _, label := range cartridgeMetadataLabels(cartridge) {
		raw, err := os.ReadFile(cartridgeMetadataPath(label))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		meta := &cartridgeMetadata{UsedBytes: -1}
		for _, line := range strings.Split(string(raw), "\n") {
			key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
			if !ok {
				continue
			}
			parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
			if err != nil {
				continue
			}
			switch strings.TrimSpace(key) {
			case "capacity_bytes":
				meta.CapacityBytes = parsed
			case "used_bytes":
				meta.UsedBytes = parsed
			}
		}
		return meta, nil
	}
	return nil, nil
}

func writeCartridgeMetadata(cartridge *domain.VirtualCartridge) error {
	if cartridge == nil {
		return domain.ErrInvalidInput
	}
	dir := mediaStateDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	capacityBytes := cartridge.CapacityBytes
	usedBytes := cartridge.UsedBytes
	if existing, err := readCartridgeMetadata(cartridge); err != nil {
		return err
	} else if existing != nil {
		if capacityBytes <= 0 && existing.CapacityBytes > 0 {
			capacityBytes = existing.CapacityBytes
		}
		if usedBytes == 0 && existing.UsedBytes > 0 {
			usedBytes = existing.UsedBytes
		}
	}
	payload := "cartridge_id=" + strings.TrimSpace(cartridge.CartridgeID) + "\n" +
		"capacity_bytes=" + strconv.FormatInt(capacityBytes, 10) + "\n" +
		"used_bytes=" + strconv.FormatInt(usedBytes, 10) + "\n"
	for _, label := range cartridgeMetadataLabels(cartridge) {
		if err := writeAtomicText(cartridgeMetadataPath(label), payload); err != nil {
			return err
		}
	}
	return nil
}

func removeCartridgeMetadataFile(cartridge *domain.VirtualCartridge) error {
	for _, label := range cartridgeMetadataLabels(cartridge) {
		path := cartridgeMetadataPath(label)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func writeAtomicText(targetPath, payload string) error {
	tmpPath := targetPath + ".tmp"
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(payload); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, targetPath); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return syncParentDir(targetPath)
}

func syncParentDir(path string) error {
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}

func mediaStateDir() string {
	if raw := strings.TrimSpace(os.Getenv("HOLO_MEDIA_STATE_DIR")); raw != "" {
		return raw
	}
	return "/run/holo/media-state"
}

func sanitizeStateID(raw string) string {
	raw = strings.TrimSpace(strings.ToLower(raw))
	if raw == "" {
		return "unknown"
	}
	var b strings.Builder
	for _, ch := range raw {
		if (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '-' || ch == '_' {
			b.WriteRune(ch)
		} else {
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "" {
		return "unknown"
	}
	return out
}

func removeCartridgeLayoutArtifacts(cartridge *domain.VirtualCartridge) error {
	if cartridge == nil {
		return domain.ErrInvalidInput
	}

	targets, err := cartridgeLayoutArtifactDirs(cartridge)
	if err != nil {
		return err
	}
	approvedRoots := approvedCartridgeLayoutRoots(cartridge.PoolID)
	for target := range targets {
		if strings.TrimSpace(target) == "" {
			continue
		}
		if err := removeAllApprovedLayoutPath(target, approvedRoots); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func resetCartridgeLayoutArtifacts(cartridge *domain.VirtualCartridge) error {
	if cartridge == nil {
		return domain.ErrInvalidInput
	}
	targets, err := cartridgeLayoutArtifactDirs(cartridge)
	if err != nil {
		return err
	}
	for target := range targets {
		if strings.TrimSpace(target) == "" {
			continue
		}
		entries, err := os.ReadDir(target)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		for _, entry := range entries {
			if entry.IsDir() || !isShortEraseLayoutArtifact(entry.Name()) {
				continue
			}
			path := filepath.Join(target, entry.Name())
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return nil
}

func cartridgeLayoutArtifactDirs(cartridge *domain.VirtualCartridge) (map[string]struct{}, error) {
	candidateRoots := approvedCartridgeLayoutRoots(cartridge.PoolID)

	targets := make(map[string]struct{})
	for _, root := range candidateRoots {
		root = strings.TrimSpace(root)
		if root == "" {
			continue
		}
		targets[storageutil.CanonicalCartridgeLayoutDir(root, cartridge.LibraryID, cartridge.CartridgeID)] = struct{}{}
		legacyDirs, err := storageutil.LegacyCartridgeLayoutDirs(root, cartridge.CartridgeID)
		if err != nil {
			return nil, err
		}
		for _, dir := range legacyDirs {
			targets[dir] = struct{}{}
		}
	}
	return targets, nil
}

func approvedCartridgeLayoutRoots(poolID string) []string {
	candidateRoots := []string{
		storageutil.PoolStorageRoot(poolID),
		storageutil.ResolveStorageRoot(),
		"/var/lib/holo/storage",
		"/tmp/holo-storage",
	}
	if home, homeErr := os.UserHomeDir(); homeErr == nil {
		candidateRoots = append(candidateRoots, filepath.Join(home, ".local", "share", "holo", "storage"))
	}

	return candidateRoots
}

func removeAllApprovedLayoutPath(target string, approvedRoots []string) error {
	target = filepath.Clean(strings.TrimSpace(target))
	if target == "" || !filepath.IsAbs(target) {
		return domain.ErrInvalidInput
	}
	if _, err := os.Lstat(target); os.IsNotExist(err) {
		return nil
	}
	if evaluated, err := filepath.EvalSymlinks(target); err == nil {
		target = filepath.Clean(evaluated)
	}
	for _, root := range approvedRoots {
		root = strings.TrimSpace(root)
		if root == "" {
			continue
		}
		root = filepath.Clean(root)
		if evaluatedRoot, err := filepath.EvalSymlinks(root); err == nil {
			root = filepath.Clean(evaluatedRoot)
		}
		if target != root && pathWithinBase(root, target) {
			return os.RemoveAll(target)
		}
	}
	return domain.ErrInvalidInput
}

func isShortEraseLayoutArtifact(name string) bool {
	switch name {
	case "data.segment",
		"metadata.segment",
		"blk_map.segment",
		"lookup.segment",
		"reclaim.segment",
		"dedup.segment",
		"segment_index.segment",
		"filemarks.state",
		"usage.counters":
		return true
	}
	return strings.HasPrefix(name, "data_") && strings.HasSuffix(name, ".seg")
}

func (h *ResourcesHandler) deleteLibraryCascade(ctx context.Context, libraryID string) error {
	if _, err := h.repo.FindLibrary(ctx, libraryID); err != nil {
		return err
	}
	if err := h.reconcileMediaState(ctx); err != nil {
		return err
	}
	if err := h.unpublishDependentPublications(ctx, libraryID, "", ""); err != nil {
		return err
	}
	drives := h.repo.ListDrives(ctx)
	cartridges := h.repo.ListCartridges(ctx)

	for _, drive := range drives {
		if drive != nil && drive.LibraryID == libraryID && drive.MountedCartridgeID != "" {
			return domain.ErrInvalidState
		}
	}

	for _, cartridge := range cartridges {
		if cartridge == nil || cartridge.LibraryID != libraryID {
			continue
		}
		if err := removeCartridgeLayoutArtifacts(cartridge); err != nil {
			return err
		}
		if err := h.repo.DeleteCartridge(ctx, cartridge.CartridgeID); err != nil {
			return err
		}
	}

	for _, drive := range drives {
		if drive == nil || drive.LibraryID != libraryID {
			continue
		}
		if drive.MountedCartridgeID != "" {
			return domain.ErrInvalidState
		}
		if err := h.repo.DeleteDrive(ctx, drive.DriveID); err != nil {
			return err
		}
		if err := removeDriveMediaStateFile(drive.LibraryID, drive.DriveID); err != nil {
			return err
		}
	}

	if err := h.repo.DeleteLibrary(ctx, libraryID); err != nil {
		return err
	}
	return nil
}

func removeDriveMediaStateFile(libraryID, driveID string) error {
	stateKey := storageutil.MediaStateKey(strings.TrimSpace(libraryID), strings.TrimSpace(driveID))
	base := filepath.Join(mediaStateDir(), sanitizeStateID(stateKey))
	for _, suffix := range []string{".slots", ".state", ".ie"} {
		if err := os.Remove(base + suffix); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func (h *ResourcesHandler) ensureLibraryAutoPublications(ctx context.Context, libraryID string) error {
	if h.target == nil {
		return nil
	}
	libraryID = strings.TrimSpace(libraryID)
	if libraryID == "" {
		return domain.ErrInvalidInput
	}

	library, err := h.repo.FindLibrary(ctx, libraryID)
	if err != nil {
		return err
	}

	drives := make([]*domain.VirtualDrive, 0)
	for _, drive := range h.repo.ListDrives(ctx) {
		if drive != nil && strings.TrimSpace(drive.LibraryID) == libraryID {
			drives = append(drives, drive)
		}
	}
	if len(drives) == 0 {
		return nil
	}
	sort.Slice(drives, func(i, j int) bool {
		if drives[i].Slot == drives[j].Slot {
			return drives[i].DriveID < drives[j].DriveID
		}
		return drives[i].Slot < drives[j].Slot
	})

	cartridges := make([]*domain.VirtualCartridge, 0)
	for _, cartridge := range h.repo.ListCartridges(ctx) {
		if cartridge != nil && strings.TrimSpace(cartridge.LibraryID) == libraryID {
			cartridges = append(cartridges, cartridge)
		}
	}
	if len(cartridges) == 0 {
		return nil
	}
	sort.Slice(cartridges, func(i, j int) bool {
		if cartridges[i].Barcode == cartridges[j].Barcode {
			return cartridges[i].CartridgeID < cartridges[j].CartridgeID
		}
		return cartridges[i].Barcode < cartridges[j].Barcode
	})

	readyByIQN := make(map[string]*domain.TargetPublication)
	for _, publication := range h.target.ListPublications(ctx) {
		if publication == nil {
			continue
		}
		if publication.State != domain.PublicationReady && publication.State != domain.PublicationCreating {
			continue
		}
		readyByIQN[strings.TrimSpace(publication.TargetIQN)] = publication
	}

	published := make(map[string]struct{})
	firstDrive := drives[0]
	firstCartridge := cartridges[0]
	driveProfile := normalizeDeviceProfile(library.Vendor, library.DriveType, "drive")
	libraryIQN := strings.TrimSpace(library.IQN)
	if libraryIQN != "" {
		if existing := readyByIQN[libraryIQN]; existing == nil {
			if _, err := h.target.Publish(ctx, orchestration.PublishRequest{
				LibraryID:     libraryID,
				DriveID:       firstDrive.DriveID,
				CartridgeID:   firstCartridge.CartridgeID,
				TargetIQN:     libraryIQN,
				DeviceRole:    "changer",
				DeviceProfile: normalizeDeviceProfile(library.Vendor, library.LibraryType, "changer"),
				DriveProfile:  driveProfile,
				Actor:         "system",
			}); err != nil && !errors.Is(err, domain.ErrConflict) {
				return err
			}
		}
		published[libraryIQN] = struct{}{}
	}

	for idx, drive := range drives {
		if drive == nil {
			continue
		}
		driveIQN := strings.TrimSpace(drive.IQN)
		if driveIQN == "" {
			continue
		}
		if existing := readyByIQN[driveIQN]; existing == nil {
			cartridge := cartridges[idx%len(cartridges)]
			if _, err := h.target.Publish(ctx, orchestration.PublishRequest{
				LibraryID:     libraryID,
				DriveID:       drive.DriveID,
				CartridgeID:   cartridge.CartridgeID,
				TargetIQN:     driveIQN,
				DeviceRole:    "drive",
				DeviceProfile: driveProfile,
				DriveProfile:  driveProfile,
				Actor:         "system",
			}); err != nil && !errors.Is(err, domain.ErrConflict) {
				return err
			}
		}
		published[driveIQN] = struct{}{}
	}

	for iqn, publication := range readyByIQN {
		if publication == nil || strings.TrimSpace(publication.LibraryID) != libraryID {
			continue
		}
		if _, keep := published[iqn]; keep {
			continue
		}
		if _, err := h.target.Unpublish(ctx, publication.PublicationID, "system"); err != nil && !errors.Is(err, domain.ErrNotFound) {
			return err
		}
	}
	return nil
}

func (h *ResourcesHandler) ensureUpgradeRuntimePublications(ctx context.Context) error {
	if h.target == nil {
		return nil
	}
	for _, publication := range h.target.ListPublications(ctx) {
		if publication == nil {
			continue
		}
		if publication.State == domain.PublicationReady || publication.State == domain.PublicationCreating {
			return nil
		}
	}
	for _, library := range h.repo.ListLibraries(ctx) {
		if library == nil {
			continue
		}
		if err := h.ensureLibraryAutoPublications(ctx, library.LibraryID); err != nil {
			return err
		}
	}
	return nil
}

func normalizeDeviceProfile(vendor, model, fallback string) string {
	vendorToken := sanitizeProfileToken(vendor)
	modelToken := sanitizeProfileToken(model)
	if mapped := profileAliasFor(modelToken); mapped != "" {
		return mapped
	}
	if vendorToken == "" && modelToken == "" {
		return fallback
	}
	if vendorToken == "" {
		return modelToken
	}
	if modelToken == "" {
		return vendorToken
	}
	return vendorToken + "-" + modelToken
}

func profileAliasFor(modelToken string) string {
	switch modelToken {
	case "ibm-ts2230", "ibm-ult3580-td3", "ult3580-td3":
		return "ibm-ult3580-td3"
	case "ibm-ts2240", "ibm-ult3580-td4", "ult3580-td4":
		return "ibm-ult3580-td4"
	case "ibm-ts2250", "ibm-ult3580-td5", "ult3580-td5":
		return "ibm-ult3580-td5"
	case "ibm-ts2260", "ibm-ult3580-td6", "ult3580-td6":
		return "ibm-ult3580-td6"
	case "ibm-ts2270", "ibm-ult3580-td7", "ult3580-td7":
		return "ibm-ult3580-td7"
	case "ibm-ts2280", "ibm-ult3580-td8", "ult3580-td8":
		return "ibm-ult3580-td8"
	case "ibm-ts2290", "ibm-ult3580-td9", "ult3580-td9":
		return "ibm-ult3580-td9"
	case "ibm-lto-10-tape-drive", "ibm-ult3580-tda", "ult3580-tda":
		return "ibm-ult3580-tda"
	case "hp-ultrium-920", "hp-ultrium-960", "hp-ultrium-3-scsi", "ultrium-3-scsi":
		return "hp-ultrium-3-scsi"
	case "hp-ultrium-1760", "hp-ultrium-1840", "hp-ultrium-4-scsi", "ultrium-4-scsi":
		return "hp-ultrium-4-scsi"
	case "hp-ultrium-3000", "hp-ultrium-3280", "hp-ultrium-5-scsi", "ultrium-5-scsi":
		return "hp-ultrium-5-scsi"
	case "hp-storeever-ultrium-6250", "hp-storeever-ultrium-6650", "hp-ultrium-6-scsi", "ultrium-6-scsi":
		return "hp-ultrium-6-scsi"
	case "hpe-storeever-lto-7-ultrium-15000", "hpe-ultrium-7-scsi", "ultrium-7-scsi":
		return "hpe-ultrium-7-scsi"
	case "hpe-storeever-lto-8-ultrium-30750", "hpe-ultrium-8-scsi", "ultrium-8-scsi":
		return "hpe-ultrium-8-scsi"
	case "hpe-storeever-lto-9-ultrium-45000", "hpe-ultrium-9-scsi", "ultrium-9-scsi":
		return "hpe-ultrium-9-scsi"
	case "quantum-lto-3-tape-drive":
		return "quantum-ultrium-td3"
	case "quantum-lto-4-tape-drive":
		return "quantum-ultrium-td4"
	case "quantum-lto-5-tape-drive":
		return "quantum-ultrium-td5"
	case "quantum-lto-6-tape-drive":
		return "quantum-ultrium-td6"
	case "quantum-lto-7-tape-drive":
		return "quantum-ultrium-td7"
	case "quantum-lto-8-tape-drive":
		return "quantum-ultrium-td8"
	case "quantum-lto-9-tape-drive":
		return "quantum-ultrium-td9"
	case "quantum-lto-10-tape-drive":
		return "quantum-ultrium-tda"
	case "storagetek-t9840a", "t9840a":
		return "stk-t9840a"
	case "storagetek-t9840b", "t9840b":
		return "stk-t9840b"
	case "storagetek-t9840c", "t9840c":
		return "stk-t9840c"
	case "storagetek-t9840d", "t9840d":
		return "stk-t9840d"
	case "storagetek-t9940a", "t9940a":
		return "stk-t9940a"
	case "storagetek-t9940b", "t9940b":
		return "stk-t9940b"
	case "storagetek-t10000a", "t10000a":
		return "stk-t10000a"
	case "storagetek-t10000b", "t10000b":
		return "stk-t10000b"
	case "storagetek-t10000c", "t10000c":
		return "stk-t10000c"
	case "storagetek-t10000d", "t10000d":
		return "stk-t10000d"
	case "ibm-3592-j1a", "03592j1a":
		return "ibm-03592j1a"
	case "ibm-3592-e05", "03592e05":
		return "ibm-03592e05"
	case "ibm-3592-e06", "03592e06":
		return "ibm-03592e06"
	case "ibm-ts3100", "ibm-ts3200", "3573-tl":
		return "ibm-3573-tl"
	case "ibm-ts3310":
		return "ibm-ts3310"
	case "ibm-ts3500", "03584l32":
		return "ibm-03584l32"
	case "ibm-ts4300":
		return "ibm-ts4300"
	case "ibm-ts4500":
		return "ibm-ts4500"
	case "ibm-diamondback":
		return "ibm-diamondback"
	case "hp-esl9000-series":
		return "hp-esl9000-series"
	case "hp-esl-e-series":
		return "hp-esl-e-series"
	case "hp-eml-e-series":
		return "hp-eml-e-series"
	case "hp-msl-g3-series", "hp-hpe-msl2024", "hp-hpe-msl4048", "hp-hpe-msl8096":
		return "hp-msl-g3-series"
	case "hp-msl6000-series":
		return "hp-msl6000-series"
	case "hpe-msl3040":
		return "hpe-msl3040"
	case "hpe-msl6480":
		return "hpe-msl6480"
	case "adic-scalar-24":
		return "adic-scalar-24"
	case "adic-scalar-100":
		return "adic-scalar-100"
	case "quantum-scalar-i500", "adic-scalar-i500":
		return "adic-scalar-i500"
	case "adic-scalar-i2000":
		return "adic-scalar-i2000"
	case "quantum-scalar-i40":
		return "quantum-scalar-i40"
	case "quantum-scalar-i80":
		return "quantum-scalar-i80"
	case "quantum-scalar-i6000":
		return "quantum-scalar-i6000"
	case "quantum-scalar-i3":
		return "quantum-scalar-i3"
	case "quantum-scalar-i6":
		return "quantum-scalar-i6"
	case "quantum-superloader-3":
		return "quantum-superloader-3"
	case "dell-powervault-tl1000":
		return "dell-tl1000"
	case "dell-powervault-tl2000":
		return "dell-tl2000"
	case "dell-powervault-tl4000":
		return "dell-tl4000"
	case "dell-emc-ml3":
		return "dell-ml3"
	case "dell-powervault-ml6000":
		return "dell-ml6000"
	case "storagetek-l20":
		return "stk-l20"
	case "storagetek-l80":
		return "stk-l80"
	case "storagetek-l700":
		return "stk-l700"
	case "storagetek-sl150":
		return "stk-sl150"
	case "storagetek-sl3000":
		return "stk-sl3000"
	case "storagetek-sl4000":
		return "stk-sl4000"
	case "storagetek-sl8500":
		return "stk-sl8500"
	case "spectra-t50e":
		return "spectra-t50e"
	case "spectra-t120":
		return "spectra-t120"
	case "spectra-t200":
		return "spectra-t200"
	case "spectra-t380":
		return "spectra-t380"
	case "spectra-t680":
		return "spectra-t680"
	case "spectra-t950":
		return "spectra-t950"
	case "spectra-t950v":
		return "spectra-t950v"
	case "spectra-tfinity":
		return "spectra-tfinity"
	case "spectra-tfinity-exascale":
		return "spectra-tfinity-exascale"
	case "spectra-stack":
		return "spectra-stack"
	case "spectra-python":
		return "spectra-python"
	case "overland-neo-series":
		return "overland-neo-series"
	case "overland-flexstor-ii":
		return "overland-flexstor-ii"
	case "overland-neoxl-multistak":
		return "overland-neoxl-multistak"
	case "tandberg-neos-t24":
		return "tandberg-neos-t24"
	case "tandberg-neoxl-40":
		return "tandberg-neoxl-40"
	case "tandberg-neoxl-80":
		return "tandberg-neoxl-80"
	default:
		return ""
	}
}

func sanitizeProfileToken(raw string) string {
	raw = strings.ToLower(strings.TrimSpace(raw))
	var b strings.Builder
	for _, ch := range raw {
		if (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') {
			b.WriteRune(ch)
			continue
		}
		if ch == '-' || ch == '_' || ch == '.' || ch == ' ' {
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

func (h *ResourcesHandler) unpublishDependentPublications(ctx context.Context, libraryID, driveID, cartridgeID string) error {
	if h.target == nil {
		return nil
	}
	libraryID = strings.TrimSpace(libraryID)
	driveID = strings.TrimSpace(driveID)
	cartridgeID = strings.TrimSpace(cartridgeID)
	for _, publication := range h.target.ListPublications(ctx) {
		if publication == nil {
			continue
		}
		match := false
		if libraryID != "" && publication.LibraryID == libraryID {
			match = true
		}
		if driveID != "" && publication.DriveID == driveID {
			match = true
		}
		if cartridgeID != "" && publication.CartridgeID == cartridgeID {
			match = true
		}
		if !match {
			continue
		}
		if _, err := h.target.Unpublish(ctx, publication.PublicationID, "system"); err != nil && !errors.Is(err, domain.ErrNotFound) {
			return err
		}
	}
	return nil
}
