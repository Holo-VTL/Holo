package orchestration

import (
	"context"
	"sort"
	"strings"
	"sync"

	"github.com/Holo-VTL/Holo/control-plane/internal/audit"
	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
)

type TargetDiscoveryService struct {
	runtimeRepo TargetRuntimeRepository
	auditW      audit.Writer
	security    *ISCSISecurityService

	mu              sync.RWMutex
	totalQueries    int
	lastVisible     int
	discoverableNow int
}

func (s *TargetDiscoveryService) SetISCSISecurityService(service *ISCSISecurityService) {
	s.security = service
}

func NewTargetDiscoveryService(runtimeRepo TargetRuntimeRepository, auditW audit.Writer) *TargetDiscoveryService {
	return &TargetDiscoveryService{runtimeRepo: runtimeRepo, auditW: auditW}
}

func (s *TargetDiscoveryService) Discover(ctx context.Context, req domain.TargetDiscoveryRequest) ([]domain.DiscoverableTarget, error) {
	req.Initiator = strings.TrimSpace(req.Initiator)
	req.Portal = strings.TrimSpace(req.Portal)
	if err := req.Validate(); err != nil {
		return nil, err
	}

	publications := s.runtimeRepo.ListDiscoverablePublications(ctx)
	results := make([]domain.DiscoverableTarget, 0, len(publications))
	for _, publication := range publications {
		if req.Portal != "" && publication.Portal != req.Portal {
			continue
		}
		allowed, err := s.initiatorAllowed(ctx, publication.TargetIQN, req.Initiator)
		if err != nil {
			return nil, err
		}
		if !allowed {
			continue
		}

		results = append(results, domain.DiscoverableTarget{
			PublicationID: publication.PublicationID,
			TargetIQN:     publication.TargetIQN,
			Portal:        publication.Portal,
			State:         string(publication.State),
		})
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].PublicationID < results[j].PublicationID
	})
	s.recordQuery(len(results), len(publications))
	audit.EmitTargetDiscoveryEvent(ctx, s.auditW, safeActor(req.Actor), "discover_targets", "target-discovery", "success", map[string]any{"initiator": req.Initiator, "portal": req.Portal, "visibleCount": len(results), "discoverableCount": len(publications)})
	return results, nil
}

func (s *TargetDiscoveryService) VisiblePublications(ctx context.Context, initiator, actor string) ([]*domain.TargetPublication, error) {
	initiator = strings.TrimSpace(initiator)
	if initiator == "" {
		return nil, domain.ErrInvalidInput
	}

	publications := s.runtimeRepo.ListPublications(ctx)
	visible := make([]*domain.TargetPublication, 0, len(publications))
	readyTotal := 0
	for _, publication := range publications {
		if publication.State != domain.PublicationReady {
			continue
		}
		readyTotal++
		allowed, err := s.initiatorAllowed(ctx, publication.TargetIQN, initiator)
		if err != nil {
			return nil, err
		}
		if allowed {
			visible = append(visible, publication)
		}
	}

	s.recordQuery(len(visible), readyTotal)
	audit.EmitTargetDiscoveryEvent(ctx, s.auditW, safeActor(actor), "query_visible_publications", "target-publications", "success", map[string]any{"initiator": initiator, "readyPublications": readyTotal, "visiblePublications": len(visible)})
	return visible, nil
}

func (s *TargetDiscoveryService) initiatorAllowed(ctx context.Context, targetIQN, initiator string) (bool, error) {
	if domain.ValidateTargetIQN(strings.ToLower(strings.TrimSpace(initiator))) != nil {
		return false, domain.ErrInvalidInput
	}
	if s.security == nil {
		return true, nil
	}
	return s.security.InitiatorAllowed(ctx, targetIQN, initiator)
}

func (s *TargetDiscoveryService) recordQuery(visible, discoverable int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.totalQueries++
	s.lastVisible = visible
	s.discoverableNow = discoverable
}

func (s *TargetDiscoveryService) DiscoverySnapshot() TargetDiscoveryHealth {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return TargetDiscoveryHealth{
		TotalQueries:    s.totalQueries,
		LastVisible:     s.lastVisible,
		DiscoverableNow: s.discoverableNow,
	}
}
