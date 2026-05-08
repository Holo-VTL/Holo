package domain

import "time"

type InitiatorRule struct {
	RuleID        string           `json:"ruleId"`
	PublicationID string           `json:"publicationId"`
	Initiator     string           `json:"initiator"`
	Permission    PolicyPermission `json:"permission"`
	Priority      int              `json:"priority"`
	CreatedAt     time.Time        `json:"createdAt"`
}

func (r InitiatorRule) Validate() error {
	if r.PublicationID == "" || r.Initiator == "" || r.RuleID == "" {
		return ErrInvalidInput
	}
	if ValidatePermission(r.Permission) != nil {
		return ErrInvalidInput
	}
	return nil
}

type AccessPolicySnapshot struct {
	SnapshotID    string          `json:"snapshotId"`
	PublicationID string          `json:"publicationId"`
	Version       int             `json:"version"`
	Rules         []InitiatorRule `json:"rules"`
	CreatedBy     string          `json:"createdBy"`
	CreatedAt     time.Time       `json:"createdAt"`
}

type AuthorizationDecision struct {
	PublicationID string           `json:"publicationId"`
	Initiator     string           `json:"initiator"`
	Decision      PolicyPermission `json:"decision"`
	Reason        string           `json:"reason"`
	MatchedRuleID string           `json:"matchedRuleId,omitempty"`
	EvaluatedAt   time.Time        `json:"evaluatedAt"`
}
