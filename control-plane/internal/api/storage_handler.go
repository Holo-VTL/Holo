package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
	"github.com/Holo-VTL/Holo/control-plane/internal/orchestration"
)

type StorageHandler struct {
	svc storageManagementService
}

type storageManagementService interface {
	DiscoverDisks(ctx context.Context) ([]domain.StorageManagedDisk, error)
	CreatePool(ctx context.Context, req orchestration.CreateStoragePoolRequest) (*domain.StoragePoolRuntime, error)
	ListPools(ctx context.Context) []*domain.StoragePoolRuntime
	GetPool(ctx context.Context, poolID string) (*domain.StoragePoolRuntime, error)
	DeletePool(ctx context.Context, poolID, actor string) error
	AttachDisk(ctx context.Context, poolID, devicePath, actor string) (*domain.StoragePoolRuntime, error)
	DetachDisk(ctx context.Context, poolID, devicePath, actor string) (*domain.StoragePoolRuntime, error)
	GetCapacity(ctx context.Context, poolID string) (*domain.StoragePoolCapacitySnapshot, error)
}

func NewStorageHandler(svc storageManagementService) *StorageHandler {
	return &StorageHandler{svc: svc}
}

type createStoragePoolRequest struct {
	PoolID              string `json:"poolId"`
	Name                string `json:"name"`
	WarningThresholdPct int    `json:"warningThresholdPct,omitempty"`
	Actor               string `json:"actor,omitempty"`
}

type manageStorageDiskRequest struct {
	DevicePath string `json:"devicePath"`
	Actor      string `json:"actor,omitempty"`
}

func (h *StorageHandler) handleDisksDiscovery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	disks, err := h.svc.DiscoverDisks(r.Context())
	if err != nil {
		respondStorageError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"disks": disks})
}

func (h *StorageHandler) handlePools(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		respondJSON(w, http.StatusOK, h.svc.ListPools(r.Context()))
	case http.MethodPost:
		var req createStoragePoolRequest
		if err := decodeRequiredJSONBody(r, &req); err != nil {
			respondStorageError(w, err)
			return
		}
		pool, err := h.svc.CreatePool(r.Context(), orchestration.CreateStoragePoolRequest{
			PoolID:              strings.TrimSpace(req.PoolID),
			Name:                strings.TrimSpace(req.Name),
			WarningThresholdPct: req.WarningThresholdPct,
			Actor:               strings.TrimSpace(req.Actor),
		})
		if err != nil {
			respondStorageError(w, err)
			return
		}
		respondJSON(w, http.StatusCreated, pool)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *StorageHandler) handlePoolSubresource(w http.ResponseWriter, r *http.Request) {
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/storage/pools/"), "/")
	if path == "" {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	parts := strings.Split(path, "/")
	poolID := strings.TrimSpace(parts[0])
	if poolID == "" {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	switch {
	case len(parts) == 1:
		h.handlePoolByID(w, r, poolID)
	case len(parts) == 2 && parts[1] == "delete":
		h.handlePoolDeleteAction(w, r, poolID)
	case len(parts) == 2 && parts[1] == "capacity":
		h.handlePoolCapacity(w, r, poolID)
	case len(parts) == 3 && parts[1] == "disks" && (parts[2] == "attach" || parts[2] == "detach"):
		h.handlePoolDiskManagement(w, r, poolID, parts[2])
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

func (h *StorageHandler) handlePoolDeleteAction(w http.ResponseWriter, r *http.Request, poolID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	actor := strings.TrimSpace(r.URL.Query().Get("actor"))
	if err := h.svc.DeletePool(r.Context(), poolID, actor); err != nil {
		respondStorageError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *StorageHandler) handlePoolByID(w http.ResponseWriter, r *http.Request, poolID string) {
	switch r.Method {
	case http.MethodGet:
		pool, err := h.svc.GetPool(r.Context(), poolID)
		if err != nil {
			respondStorageError(w, err)
			return
		}
		respondJSON(w, http.StatusOK, pool)
	case http.MethodDelete:
		actor := strings.TrimSpace(r.URL.Query().Get("actor"))
		if err := h.svc.DeletePool(r.Context(), poolID, actor); err != nil {
			respondStorageError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *StorageHandler) handlePoolCapacity(w http.ResponseWriter, r *http.Request, poolID string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	snapshot, err := h.svc.GetCapacity(r.Context(), poolID)
	if err != nil {
		respondStorageError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, snapshot)
}

func (h *StorageHandler) handlePoolDiskManagement(w http.ResponseWriter, r *http.Request, poolID, action string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req manageStorageDiskRequest
	if err := decodeRequiredJSONBody(r, &req); err != nil {
		respondStorageError(w, err)
		return
	}
	if domain.ValidateDevicePath(req.DevicePath) != nil {
		respondStorageError(w, domain.ErrInvalidInput)
		return
	}

	var (
		pool *domain.StoragePoolRuntime
		err  error
	)
	if action == "attach" {
		pool, err = h.svc.AttachDisk(r.Context(), poolID, req.DevicePath, req.Actor)
	} else {
		pool, err = h.svc.DetachDisk(r.Context(), poolID, req.DevicePath, req.Actor)
	}
	if err != nil {
		respondStorageError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, pool)
}

func respondStorageError(w http.ResponseWriter, err error) {
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
	case errors.Is(err, domain.ErrCapacityExceeded):
		status = http.StatusInsufficientStorage
		message = "insufficient storage capacity"
	}
	if status == http.StatusInternalServerError {
		respondError(w, status, message, err)
		return
	}
	respondError(w, status, message, nil)
}
