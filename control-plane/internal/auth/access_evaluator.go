package auth

import (
	"strings"
	"time"

	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
)

type AccessEvaluator struct{}

func NewAccessEvaluator() *AccessEvaluator { return &AccessEvaluator{} }

func (e *AccessEvaluator) IsAllowed(subject string, policies []domain.TargetAccessPolicy) bool {
	allowed := false
	for _, p := range policies {
		if p.Subject == subject {
			if p.Permission == domain.PermissionDeny {
				return false
			}
			if p.Permission == domain.PermissionAllow {
				allowed = true
			}
		}
	}
	return allowed
}

func (e *AccessEvaluator) EvaluateInitiator(publicationID, initiator string, rules []domain.InitiatorRule) domain.AuthorizationDecision {
	now := time.Now().UTC()
	normalizedInitiator := normalizeInitiator(initiator)
	if normalizedInitiator == "" {
		return domain.AuthorizationDecision{
			PublicationID: publicationID,
			Initiator:     initiator,
			Decision:      domain.PermissionDeny,
			Reason:        "invalid_initiator",
			EvaluatedAt:   now,
		}
	}

	var (
		bestPriority int
		found        bool
		candidates   []domain.InitiatorRule
	)
	for _, rule := range rules {
		if !matchesInitiator(rule.Initiator, normalizedInitiator) {
			continue
		}
		if !found || rule.Priority > bestPriority {
			bestPriority = rule.Priority
			candidates = candidates[:0]
			candidates = append(candidates, rule)
			found = true
			continue
		}
		if rule.Priority == bestPriority {
			candidates = append(candidates, rule)
		}
	}

	if !found {
		return domain.AuthorizationDecision{
			PublicationID: publicationID,
			Initiator:     initiator,
			Decision:      domain.PermissionDeny,
			Reason:        "default_deny_no_match",
			EvaluatedAt:   now,
		}
	}

	chosen := candidates[0]
	for i := 1; i < len(candidates); i++ {
		chosen = chooseDeterministicRule(chosen, candidates[i])
	}

	decision := chosen.Permission
	reason := "matched_allow_rule"
	if decision == domain.PermissionDeny {
		reason = "matched_deny_rule"
	}
	if decision != domain.PermissionAllow && decision != domain.PermissionDeny {
		decision = domain.PermissionDeny
		reason = "invalid_rule_permission"
	}

	return domain.AuthorizationDecision{
		PublicationID: publicationID,
		Initiator:     initiator,
		Decision:      decision,
		Reason:        reason,
		MatchedRuleID: chosen.RuleID,
		EvaluatedAt:   now,
	}
}

func chooseDeterministicRule(a, b domain.InitiatorRule) domain.InitiatorRule {
	if a.Permission == domain.PermissionDeny && b.Permission != domain.PermissionDeny {
		return a
	}
	if b.Permission == domain.PermissionDeny && a.Permission != domain.PermissionDeny {
		return b
	}
	if a.RuleID == "" {
		return b
	}
	if b.RuleID == "" {
		return a
	}
	if b.RuleID < a.RuleID {
		return b
	}
	return a
}

func matchesInitiator(pattern, normalizedInitiator string) bool {
	normalizedPattern := normalizeInitiator(pattern)
	if normalizedPattern == "" {
		return false
	}
	if normalizedPattern == "*" || normalizedPattern == "all" {
		return true
	}
	return normalizedPattern == normalizedInitiator
}

func normalizeInitiator(v string) string {
	return strings.ToLower(strings.TrimSpace(v))
}
