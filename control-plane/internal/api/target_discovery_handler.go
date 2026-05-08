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

func (h *TargetDiscoveryHandler) handleDiscovery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	req := domain.TargetDiscoveryRequest{
		Initiator: r.URL.Query().Get("initiator"),
		Actor:     r.URL.Query().Get("actor"),
		Portal:    r.URL.Query().Get("portal"),
	}
	results, err := h.service.Discover(r.Context(), req)
	if err != nil {
		respondAccessError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, discoverTargetsResponse{
		Initiator: req.Initiator,
		Portal:    req.Portal,
		Targets:   results,
	})
}
