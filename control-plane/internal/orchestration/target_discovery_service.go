package orchestration

import (
	"context"
	"sort"
	"strings"
	"sync"

	"github.com/Holo-VTL/Holo/control-plane/internal/audit"
	"github.com/Holo-VTL/Holo/control-plane/internal/auth"
	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
	"github.com/Holo-VTL/Holo/control-plane/internal/repo/memory"
)

type TargetDiscoveryService struct {
	runtimeRepo TargetRuntimeRepository
	accessRepo  *memory.TargetAccessRepo
	evaluator   *auth.AccessEvaluator
	auditW      audit.Writer

	mu              sync.RWMutex
	totalQueries    int
	lastVisible     int
	discoverableNow int
}

func NewTargetDiscoveryService(runtimeRepo TargetRuntimeRepository, accessRepo *memory.TargetAccessRepo, evaluator *auth.AccessEvaluator, auditW audit.Writer) *TargetDiscoveryService {
	return &TargetDiscoveryService{
		runtimeRepo: runtimeRepo,
		accessRepo:  accessRepo,
		evaluator:   evaluator,
		auditW:      auditW,
	}
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

		rules, err := s.accessRepo.CurrentRules(ctx, publication.PublicationID)
		if err != nil {
			return nil, err
		}
		decision := s.evaluator.EvaluateInitiator(publication.PublicationID, req.Initiator, rules)
		if decision.Decision != domain.PermissionAllow {
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

	s.mu.Lock()
	s.totalQueries++
	s.lastVisible = len(results)
	s.discoverableNow = len(publications)
	s.mu.Unlock()

	audit.EmitTargetDiscoveryEvent(ctx, s.auditW, safeActor(req.Actor), "discover_targets", "target-discovery", "success", map[string]any{"initiator": req.Initiator, "portal": req.Portal, "visibleCount": len(results), "discoverableCount": len(publications)})
	return results, nil
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
