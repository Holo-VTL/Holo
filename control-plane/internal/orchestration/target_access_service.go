package orchestration

import (
	"context"
	"strings"
	"time"

	"github.com/Holo-VTL/Holo/control-plane/internal/audit"
	"github.com/Holo-VTL/Holo/control-plane/internal/auth"
	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
	"github.com/Holo-VTL/Holo/control-plane/internal/repo/memory"
)

type TargetAccessService struct {
	runtimeRepo TargetRuntimeRepository
	accessRepo  *memory.TargetAccessRepo
	evaluator   *auth.AccessEvaluator
	auditW      audit.Writer
}

func NewTargetAccessService(runtimeRepo TargetRuntimeRepository, accessRepo *memory.TargetAccessRepo, evaluator *auth.AccessEvaluator, auditW audit.Writer) *TargetAccessService {
	return &TargetAccessService{
		runtimeRepo: runtimeRepo,
		accessRepo:  accessRepo,
		evaluator:   evaluator,
		auditW:      auditW,
	}
}

func (s *TargetAccessService) ReplaceRules(ctx context.Context, publicationID, actor string, rules []domain.InitiatorRule) (domain.AccessPolicySnapshot, error) {
	if strings.TrimSpace(publicationID) == "" {
		return domain.AccessPolicySnapshot{}, domain.ErrInvalidInput
	}
	if _, err := s.runtimeRepo.FindPublication(ctx, publicationID); err != nil {
		return domain.AccessPolicySnapshot{}, err
	}

	normalized := make([]domain.InitiatorRule, 0, len(rules))
	for _, rule := range rules {
		rule.PublicationID = publicationID
		rule.Initiator = strings.TrimSpace(rule.Initiator)
		if rule.Initiator == "" {
			return domain.AccessPolicySnapshot{}, domain.ErrInvalidInput
		}
		normalized = append(normalized, rule)
	}

	snapshot, err := s.accessRepo.ReplaceRules(ctx, publicationID, safeActor(actor), normalized)
	if err != nil {
		return domain.AccessPolicySnapshot{}, err
	}
	audit.EmitTargetAccessPolicyEvent(ctx, s.auditW, safeActor(actor), "access_rules_replaced", publicationID, "success", map[string]any{"snapshotId": snapshot.SnapshotID, "version": snapshot.Version, "ruleCount": len(snapshot.Rules)})
	return snapshot, nil
}

func (s *TargetAccessService) ListRules(ctx context.Context, publicationID string) ([]domain.InitiatorRule, error) {
	if strings.TrimSpace(publicationID) == "" {
		return nil, domain.ErrInvalidInput
	}
	if _, err := s.runtimeRepo.FindPublication(ctx, publicationID); err != nil {
		return nil, err
	}
	return s.accessRepo.CurrentRules(ctx, publicationID)
}

func (s *TargetAccessService) Authorize(ctx context.Context, publicationID, initiator, actor string) (domain.AuthorizationDecision, error) {
	if strings.TrimSpace(publicationID) == "" || strings.TrimSpace(initiator) == "" {
		return domain.AuthorizationDecision{}, domain.ErrInvalidInput
	}
	publication, err := s.runtimeRepo.FindPublication(ctx, publicationID)
	if err != nil {
		return domain.AuthorizationDecision{}, err
	}

	decision := domain.AuthorizationDecision{}
	if publication.State != domain.PublicationReady {
		decision = domain.AuthorizationDecision{
			PublicationID: publicationID,
			Initiator:     initiator,
			Decision:      domain.PermissionDeny,
			Reason:        "publication_not_ready",
			EvaluatedAt:   time.Now().UTC(),
		}
	} else {
		rules, err := s.accessRepo.CurrentRules(ctx, publicationID)
		if err != nil {
			return domain.AuthorizationDecision{}, err
		}
		decision = s.evaluator.EvaluateInitiator(publicationID, initiator, rules)
	}

	audit.EmitTargetAccessPolicyEvent(ctx, s.auditW, safeActor(actor), "authorize_initiator", publicationID, "success", map[string]any{"initiator": initiator, "decision": decision.Decision, "reason": decision.Reason, "matchedRuleId": decision.MatchedRuleID})
	return decision, nil
}

func (s *TargetAccessService) ListVisiblePublications(ctx context.Context, initiator, actor string) ([]*domain.TargetPublication, error) {
	if strings.TrimSpace(initiator) == "" {
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
		rules, err := s.accessRepo.CurrentRules(ctx, publication.PublicationID)
		if err != nil {
			return nil, err
		}
		decision := s.evaluator.EvaluateInitiator(publication.PublicationID, initiator, rules)
		if decision.Decision == domain.PermissionAllow {
			visible = append(visible, publication)
		}
	}

	audit.EmitTargetAccessPolicyEvent(ctx, s.auditW, safeActor(actor), "query_visible_publications", "target-publications", "success", map[string]any{"initiator": initiator, "readyPublications": readyTotal, "visiblePublications": len(visible)})
	return visible, nil
}

func (s *TargetAccessService) RollbackRules(ctx context.Context, publicationID, actor string) (domain.AccessPolicySnapshot, bool, error) {
	if strings.TrimSpace(publicationID) == "" {
		return domain.AccessPolicySnapshot{}, false, domain.ErrInvalidInput
	}
	if _, err := s.runtimeRepo.FindPublication(ctx, publicationID); err != nil {
		return domain.AccessPolicySnapshot{}, false, err
	}
	snapshot, noop, err := s.accessRepo.Rollback(ctx, publicationID, safeActor(actor))
	if err != nil {
		return domain.AccessPolicySnapshot{}, false, err
	}
	audit.EmitTargetAccessPolicyEvent(ctx, s.auditW, safeActor(actor), "rollback_access_rules", publicationID, "success", map[string]any{"snapshotId": snapshot.SnapshotID, "version": snapshot.Version, "noop": noop})
	return snapshot, noop, nil
}

func (s *TargetAccessService) AccessPolicySnapshotCount() int {
	if s.accessRepo == nil {
		return 0
	}
	return s.accessRepo.SnapshotCount()
}
