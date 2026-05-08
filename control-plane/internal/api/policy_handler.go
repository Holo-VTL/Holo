package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
)

var (
	errAccessPolicyRepoNil    = errors.New("access policy repository is not configured")
	errRetentionPolicyRepoNil = errors.New("retention policy repository is not configured")
)

type PolicyHandler struct {
	accessRepo    accessPolicyRepo
	retentionRepo retentionPolicyRepo
}

type accessPolicyRepo interface {
	Save(ctx context.Context, p domain.TargetAccessPolicy) error
}

type retentionPolicyRepo interface {
	Save(ctx context.Context, p domain.RetentionPolicy) error
}

func NewPolicyHandler(accessRepo accessPolicyRepo, retentionRepo retentionPolicyRepo) *PolicyHandler {
	return &PolicyHandler{
		accessRepo:    accessRepo,
		retentionRepo: retentionRepo,
	}
}

func (h *PolicyHandler) handleCreateAccessPolicy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req domain.TargetAccessPolicy
	if err := decodeRequiredJSONBody(r, &req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body", err)
		return
	}
	if err := req.Validate(); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body", err)
		return
	}
	if h.accessRepo == nil {
		respondError(w, http.StatusInternalServerError, "internal server error", errAccessPolicyRepoNil)
		return
	}
	if err := h.accessRepo.Save(r.Context(), req); err != nil {
		respondError(w, http.StatusInternalServerError, "internal server error", err)
		return
	}
	respondJSON(w, http.StatusCreated, req)
}

func (h *PolicyHandler) handleCreateRetentionPolicy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req domain.RetentionPolicy
	if err := decodeRequiredJSONBody(r, &req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body", err)
		return
	}
	if err := req.Validate(); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body", err)
		return
	}
	if h.retentionRepo == nil {
		respondError(w, http.StatusInternalServerError, "internal server error", errRetentionPolicyRepoNil)
		return
	}
	if err := h.retentionRepo.Save(r.Context(), req); err != nil {
		respondError(w, http.StatusInternalServerError, "internal server error", err)
		return
	}
	respondJSON(w, http.StatusCreated, req)
}
