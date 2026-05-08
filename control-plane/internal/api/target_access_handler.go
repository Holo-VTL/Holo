package api

import (
	"errors"
	"net/http"

	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
	"github.com/Holo-VTL/Holo/control-plane/internal/orchestration"
)

type TargetAccessHandler struct {
	service *orchestration.TargetAccessService
}

func NewTargetAccessHandler(service *orchestration.TargetAccessService) *TargetAccessHandler {
	return &TargetAccessHandler{service: service}
}

type initiatorRuleInput struct {
	RuleID     string                  `json:"ruleId,omitempty"`
	Initiator  string                  `json:"initiator"`
	Permission domain.PolicyPermission `json:"permission"`
	Priority   int                     `json:"priority"`
}

type replaceAccessRulesRequest struct {
	Actor string               `json:"actor"`
	Rules []initiatorRuleInput `json:"rules"`
}

type authorizeRequest struct {
	Initiator string `json:"initiator"`
	Actor     string `json:"actor"`
}

type rollbackAccessRequest struct {
	Actor string `json:"actor"`
}

type visibilityResponse struct {
	Initiator    string                      `json:"initiator"`
	Publications []*domain.TargetPublication `json:"publications"`
}

func (h *TargetAccessHandler) handleAccessRules(w http.ResponseWriter, r *http.Request, publicationID string) {
	switch r.Method {
	case http.MethodGet:
		rules, err := h.service.ListRules(r.Context(), publicationID)
		if err != nil {
			respondAccessError(w, err)
			return
		}
		respondJSON(w, http.StatusOK, map[string]any{"publicationId": publicationID, "rules": rules})
	case http.MethodPost:
		var req replaceAccessRulesRequest
		if err := decodeOptionalJSONBody(r, &req); err != nil {
			respondError(w, http.StatusBadRequest, "invalid request body", err)
			return
		}
		rules := make([]domain.InitiatorRule, 0, len(req.Rules))
		for _, input := range req.Rules {
			if domain.ValidatePermission(input.Permission) != nil {
				respondAccessError(w, domain.ErrInvalidInput)
				return
			}
			rules = append(rules, domain.InitiatorRule{
				RuleID:        input.RuleID,
				PublicationID: publicationID,
				Initiator:     input.Initiator,
				Permission:    input.Permission,
				Priority:      input.Priority,
			})
		}
		snapshot, err := h.service.ReplaceRules(r.Context(), publicationID, req.Actor, rules)
		if err != nil {
			respondAccessError(w, err)
			return
		}
		respondJSON(w, http.StatusOK, snapshot)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *TargetAccessHandler) handleAuthorize(w http.ResponseWriter, r *http.Request, publicationID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req authorizeRequest
	if err := decodeOptionalJSONBody(r, &req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body", err)
		return
	}
	decision, err := h.service.Authorize(r.Context(), publicationID, req.Initiator, req.Actor)
	if err != nil {
		respondAccessError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, decision)
}

func (h *TargetAccessHandler) handleAccessRollback(w http.ResponseWriter, r *http.Request, publicationID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	actor := r.URL.Query().Get("actor")
	var req rollbackAccessRequest
	if err := decodeOptionalJSONBody(r, &req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body", err)
		return
	}
	if req.Actor != "" {
		actor = req.Actor
	}
	snapshot, noop, err := h.service.RollbackRules(r.Context(), publicationID, actor)
	if err != nil {
		respondAccessError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"snapshot": snapshot, "noop": noop})
}

func (h *TargetAccessHandler) handleVisible(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	initiator := r.URL.Query().Get("initiator")
	actor := r.URL.Query().Get("actor")
	publications, err := h.service.ListVisiblePublications(r.Context(), initiator, actor)
	if err != nil {
		respondAccessError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, visibilityResponse{Initiator: initiator, Publications: publications})
}

func respondAccessError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	message := "internal server error"
	switch {
	case errors.Is(err, domain.ErrInvalidInput), errors.Is(err, domain.ErrInvalidState):
		status = http.StatusBadRequest
		message = "invalid request"
	case errors.Is(err, domain.ErrNotFound):
		status = http.StatusNotFound
		message = "resource not found"
	case errors.Is(err, domain.ErrUnauthorized):
		status = http.StatusForbidden
		message = "forbidden"
	case errors.Is(err, domain.ErrConflict):
		status = http.StatusConflict
		message = "resource conflict"
	}
	http.Error(w, message, status)
}
