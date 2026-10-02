package api

import (
	"net/http"

	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
	"github.com/Holo-VTL/Holo/control-plane/internal/orchestration"
)

type TargetDiscoveryHandler struct {
	service *orchestration.TargetDiscoveryService
}

func NewTargetDiscoveryHandler(service *orchestration.TargetDiscoveryService) *TargetDiscoveryHandler {
	return &TargetDiscoveryHandler{service: service}
}

type discoverTargetsResponse struct {
	Initiator string                      `json:"initiator"`
	Portal    string                      `json:"portal,omitempty"`
	Targets   []domain.DiscoverableTarget `json:"targets"`
}

type visibleTargetsResponse struct {
	Initiator    string                      `json:"initiator"`
	Publications []*domain.TargetPublication `json:"publications"`
}

func (h *TargetDiscoveryHandler) handleDiscovery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "method not allowed", nil)
		return
	}
	actor, err := selfAssertedAuditActor(r.URL.Query().Get("actor"))
	if err != nil {
		respondResourceError(w, err)
		return
	}

	req := domain.TargetDiscoveryRequest{
		Initiator: r.URL.Query().Get("initiator"),
		Actor:     actor,
		Portal:    r.URL.Query().Get("portal"),
	}
	results, err := h.service.Discover(r.Context(), req)
	if err != nil {
		respondResourceError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, discoverTargetsResponse{
		Initiator: req.Initiator,
		Portal:    req.Portal,
		Targets:   results,
	})
}

func (h *TargetDiscoveryHandler) handleVisible(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "method not allowed", nil)
		return
	}
	actor, err := selfAssertedAuditActor(r.URL.Query().Get("actor"))
	if err != nil {
		respondResourceError(w, err)
		return
	}
	initiator := r.URL.Query().Get("initiator")
	publications, err := h.service.VisiblePublications(r.Context(), initiator, actor)
	if err != nil {
		respondResourceError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, visibleTargetsResponse{Initiator: initiator, Publications: publications})
}
