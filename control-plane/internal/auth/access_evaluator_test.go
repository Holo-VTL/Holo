package auth

import (
	"testing"

	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
)

func TestEvaluateInitiatorDefaultDenyWhenNoRules(t *testing.T) {
	evaluator := NewAccessEvaluator()
	decision := evaluator.EvaluateInitiator("pub-1", "iqn.1993-08.org.debian:01:test", nil)
	if decision.Decision != domain.PermissionDeny {
		t.Fatalf("expected deny, got %s", decision.Decision)
	}
	if decision.Reason != "default_deny_no_match" {
		t.Fatalf("unexpected reason: %s", decision.Reason)
	}
}

func TestEvaluateInitiatorDenyPrecedenceAtSamePriority(t *testing.T) {
	evaluator := NewAccessEvaluator()
	rules := []domain.InitiatorRule{
		{RuleID: "rule-allow", PublicationID: "pub-1", Initiator: "iqn.1993-08.org.debian:01:test", Permission: domain.PermissionAllow, Priority: 100},
		{RuleID: "rule-deny", PublicationID: "pub-1", Initiator: "iqn.1993-08.org.debian:01:test", Permission: domain.PermissionDeny, Priority: 100},
	}

	decision := evaluator.EvaluateInitiator("pub-1", "iqn.1993-08.org.debian:01:test", rules)
	if decision.Decision != domain.PermissionDeny {
		t.Fatalf("expected deny precedence, got %s", decision.Decision)
	}
	if decision.MatchedRuleID != "rule-deny" {
		t.Fatalf("expected deny rule match, got %s", decision.MatchedRuleID)
	}
}

func TestEvaluateInitiatorMatchesWildcardRule(t *testing.T) {
	evaluator := NewAccessEvaluator()
	rules := []domain.InitiatorRule{
		{RuleID: "rule-any", PublicationID: "pub-1", Initiator: "ALL", Permission: domain.PermissionAllow, Priority: 1},
	}

	decision := evaluator.EvaluateInitiator("pub-1", "iqn.1993-08.org.debian:01:abc", rules)
	if decision.Decision != domain.PermissionAllow {
		t.Fatalf("expected wildcard allow, got %s", decision.Decision)
	}
}

func TestEvaluateInitiatorPriorityOverride(t *testing.T) {
	evaluator := NewAccessEvaluator()
	rules := []domain.InitiatorRule{
		{RuleID: "rule-deny", PublicationID: "pub-1", Initiator: "iqn.1993-08.org.debian:01:test", Permission: domain.PermissionDeny, Priority: 10},
		{RuleID: "rule-allow", PublicationID: "pub-1", Initiator: "iqn.1993-08.org.debian:01:test", Permission: domain.PermissionAllow, Priority: 20},
	}

	decision := evaluator.EvaluateInitiator("pub-1", "iqn.1993-08.org.debian:01:test", rules)
	if decision.Decision != domain.PermissionAllow {
		t.Fatalf("expected allow from higher priority rule, got %s", decision.Decision)
	}
	if decision.MatchedRuleID != "rule-allow" {
		t.Fatalf("expected rule-allow match, got %s", decision.MatchedRuleID)
	}
}
