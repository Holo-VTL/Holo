package api

import (
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
	"github.com/Holo-VTL/Holo/control-plane/internal/orchestration"
)

type TargetHandler struct {
	service *orchestration.TargetRuntimeService
	access  *TargetAccessHandler
}

func NewTargetHandler(service *orchestration.TargetRuntimeService, access *TargetAccessHandler) *TargetHandler {
	return &TargetHandler{service: service, access: access}
}

type publishTargetRequest struct {
	PoolID        string `json:"poolId,omitempty"`
	LibraryID     string `json:"libraryId"`
	DriveID       string `json:"driveId"`
	CartridgeID   string `json:"cartridgeId"`
	TargetIQN     string `json:"targetIqn"`
	DeviceRole    string `json:"deviceRole,omitempty"`
	DeviceProfile string `json:"deviceProfile,omitempty"`
	DriveProfile  string `json:"driveProfile,omitempty"`
	Actor         string `json:"actor"`
}

type validationRunRequest struct {
	Mode    domain.ValidationMode `json:"mode"`
	Bytes   int64                 `json:"bytes,omitempty"`
	Pattern string                `json:"pattern,omitempty"`
}

func (h *TargetHandler) handlePublications(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		var req publishTargetRequest
		if err := decodeRequiredJSONBody(r, &req); err != nil {
			respondError(w, http.StatusBadRequest, "invalid request body", err)
			return
		}
		if err := validatePublishTargetRequest(req); err != nil {
			respondError(w, http.StatusBadRequest, "invalid request", err)
			return
		}
		publication, err := h.service.Publish(r.Context(), orchestration.PublishRequest{
			LibraryID:     req.LibraryID,
			DriveID:       req.DriveID,
			CartridgeID:   req.CartridgeID,
			TargetIQN:     req.TargetIQN,
			DeviceRole:    req.DeviceRole,
			DeviceProfile: req.DeviceProfile,
			DriveProfile:  req.DriveProfile,
			Actor:         req.Actor,
		})
		if err != nil {
			status := http.StatusInternalServerError
			message := "internal server error"
			if errors.Is(err, domain.ErrInvalidInput) {
				status = http.StatusBadRequest
				message = "invalid request"
			}
			if errors.Is(err, domain.ErrConflict) {
				status = http.StatusConflict
				message = "resource conflict"
			}
			if errors.Is(err, domain.ErrNotFound) {
				status = http.StatusNotFound
				message = "resource not found"
			}
			if errors.Is(err, domain.ErrCapacityExceeded) {
				status = http.StatusInsufficientStorage
				message = "insufficient storage capacity"
			}
			respondError(w, status, message, err)
			return
		}
		respondJSON(w, http.StatusAccepted, publication)
	case http.MethodGet:
		publications := h.service.ListPublications(r.Context())
		if strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("history")), "all") {
			respondJSON(w, http.StatusOK, publications)
			return
		}
		respondJSON(w, http.StatusOK, latestPublicationsByIQN(publications))
	default:
		respondError(w, http.StatusMethodNotAllowed, "method not allowed", nil)
	}
}

func validatePublishTargetRequest(req publishTargetRequest) error {
	if strings.TrimSpace(req.TargetIQN) != "" {
		if err := domain.ValidateTargetIQN(req.TargetIQN); err != nil {
			return err
		}
	}
	for _, value := range []string{req.LibraryID, req.DriveID, req.CartridgeID} {
		if err := validateManagementID(value); err != nil {
			return err
		}
	}
	for _, value := range []string{req.DeviceProfile, req.DriveProfile} {
		if err := validateProfileToken(value); err != nil {
			return err
		}
	}
	return nil
}

func latestPublicationsByIQN(publications []*domain.TargetPublication) []*domain.TargetPublication {
	byIQN := make(map[string]*domain.TargetPublication)
	for _, publication := range publications {
		if publication == nil {
			continue
		}
		iqn := strings.TrimSpace(publication.TargetIQN)
		if iqn == "" {
			iqn = publication.PublicationID
		}
		current, exists := byIQN[iqn]
		if !exists || preferPublication(publication, current) {
			byIQN[iqn] = publication
		}
	}

	out := make([]*domain.TargetPublication, 0, len(byIQN))
	for _, publication := range byIQN {
		out = append(out, publication)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].TargetIQN < out[j].TargetIQN
		}
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	return out
}

func preferPublication(candidate, current *domain.TargetPublication) bool {
	if publicationStateRank(candidate.State) != publicationStateRank(current.State) {
		return publicationStateRank(candidate.State) < publicationStateRank(current.State)
	}
	if !candidate.UpdatedAt.Equal(current.UpdatedAt) {
		return candidate.UpdatedAt.After(current.UpdatedAt)
	}
	return candidate.PublicationID > current.PublicationID
}

func publicationStateRank(state domain.PublicationState) int {
	switch state {
	case domain.PublicationReady:
		return 0
	case domain.PublicationCreating:
		return 1
	case domain.PublicationFailed:
		return 2
	case domain.PublicationDisabled:
		return 3
	default:
		return 4
	}
}

func (h *TargetHandler) handlePublicationSubresource(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/v1/targets/publications/")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 1 || parts[0] == "" {
		respondError(w, http.StatusBadRequest, "publication id required", nil)
		return
	}
	publicationID := parts[0]

	if len(parts) == 1 {
		h.handlePublicationByID(w, r, publicationID)
		return
	}

	switch parts[1] {
	case "delete":
		h.handleUnpublishAction(w, r, publicationID)
	case "rollback":
		h.handleRollback(w, r, publicationID)
	case "validation-runs":
		h.handleValidationRuns(w, r, publicationID)
	case "access-rules":
		if h.access == nil {
			respondError(w, http.StatusNotFound, "not found", nil)
			return
		}
		h.access.handleAccessRules(w, r, publicationID)
	case "authorize":
		if h.access == nil {
			respondError(w, http.StatusNotFound, "not found", nil)
			return
		}
		h.access.handleAuthorize(w, r, publicationID)
	case "access-rollback":
		if h.access == nil {
			respondError(w, http.StatusNotFound, "not found", nil)
			return
		}
		h.access.handleAccessRollback(w, r, publicationID)
	default:
		respondError(w, http.StatusNotFound, "not found", nil)
	}
}

func (h *TargetHandler) handleUnpublishAction(w http.ResponseWriter, r *http.Request, publicationID string) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "method not allowed", nil)
		return
	}
	actor := r.URL.Query().Get("actor")
	publication, err := h.service.Unpublish(r.Context(), publicationID, actor)
	if err != nil {
		status := http.StatusInternalServerError
		message := "internal server error"
		if errors.Is(err, domain.ErrInvalidInput) || errors.Is(err, domain.ErrInvalidState) {
			status = http.StatusBadRequest
			message = "invalid request"
		}
		if errors.Is(err, domain.ErrNotFound) {
			status = http.StatusNotFound
			message = "resource not found"
		}
		if errors.Is(err, domain.ErrCapacityExceeded) {
			status = http.StatusInsufficientStorage
			message = "insufficient storage capacity"
		}
		respondError(w, status, message, err)
		return
	}
	respondJSON(w, http.StatusAccepted, publication)
}

func (h *TargetHandler) handlePublicationByID(w http.ResponseWriter, r *http.Request, publicationID string) {
	switch r.Method {
	case http.MethodGet:
		publication, err := h.service.GetPublication(r.Context(), publicationID)
		if err != nil {
			respondError(w, http.StatusNotFound, "resource not found", nil)
			return
		}
		respondJSON(w, http.StatusOK, publication)
	case http.MethodDelete:
		actor := r.URL.Query().Get("actor")
		publication, err := h.service.Unpublish(r.Context(), publicationID, actor)
		if err != nil {
			status := http.StatusInternalServerError
			message := "internal server error"
			if errors.Is(err, domain.ErrInvalidInput) || errors.Is(err, domain.ErrInvalidState) {
				status = http.StatusBadRequest
				message = "invalid request"
			}
			if errors.Is(err, domain.ErrNotFound) {
				status = http.StatusNotFound
				message = "resource not found"
			}
			if errors.Is(err, domain.ErrCapacityExceeded) {
				status = http.StatusInsufficientStorage
				message = "insufficient storage capacity"
			}
			respondError(w, status, message, err)
			return
		}
		respondJSON(w, http.StatusAccepted, publication)
	default:
		respondError(w, http.StatusMethodNotAllowed, "method not allowed", nil)
	}
}

func (h *TargetHandler) handleRollback(w http.ResponseWriter, r *http.Request, publicationID string) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "method not allowed", nil)
		return
	}
	actor := r.URL.Query().Get("actor")
	publication, err := h.service.Rollback(r.Context(), publicationID, actor)
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid request", nil)
		return
	}
	respondJSON(w, http.StatusOK, publication)
}

func (h *TargetHandler) handleValidationRuns(w http.ResponseWriter, r *http.Request, publicationID string) {
	switch r.Method {
	case http.MethodPost:
		actor := r.URL.Query().Get("actor")
		var req validationRunRequest
		if err := decodeOptionalJSONBody(r, &req); err != nil {
			respondError(w, http.StatusBadRequest, "invalid request body", err)
			return
		}
		run, err := h.service.StartValidationRunWithRequest(r.Context(), publicationID, actor, orchestration.ValidationRunRequest{
			Mode:    req.Mode,
			Bytes:   req.Bytes,
			Pattern: req.Pattern,
		})
		if err != nil {
			status := http.StatusInternalServerError
			message := "internal server error"
			if errors.Is(err, domain.ErrInvalidInput) || errors.Is(err, domain.ErrInvalidState) {
				status = http.StatusBadRequest
				message = "invalid request"
			}
			if errors.Is(err, domain.ErrNotFound) {
				status = http.StatusNotFound
				message = "resource not found"
			}
			respondError(w, status, message, err)
			return
		}
		respondJSON(w, http.StatusAccepted, run)
	case http.MethodGet:
		runs, err := h.service.ListValidationRuns(r.Context(), publicationID)
		if err != nil {
			respondError(w, http.StatusNotFound, "resource not found", err)
			return
		}
		respondJSON(w, http.StatusOK, runs)
	default:
		respondError(w, http.StatusMethodNotAllowed, "method not allowed", nil)
	}
}
