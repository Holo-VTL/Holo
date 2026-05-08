package memory

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
)

const maxSnapshotHistory = 100

type TargetAccessRepo struct {
	mu          sync.RWMutex
	snapshots   map[string][]domain.AccessPolicySnapshot
	nextVersion map[string]int
}

func NewTargetAccessRepo() *TargetAccessRepo {
	return &TargetAccessRepo{
		snapshots:   make(map[string][]domain.AccessPolicySnapshot),
		nextVersion: make(map[string]int),
	}
}

func (r *TargetAccessRepo) ReplaceRules(_ context.Context, publicationID, actor string, rules []domain.InitiatorRule) (domain.AccessPolicySnapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	version := r.nextVersion[publicationID] + 1
	now := time.Now().UTC()
	normalized := make([]domain.InitiatorRule, 0, len(rules))
	for i, rule := range rules {
		if rule.RuleID == "" {
			rule.RuleID = fmt.Sprintf("%s-rule-%d-v%d", publicationID, i+1, version)
		}
		rule.PublicationID = publicationID
		rule.CreatedAt = now
		if err := rule.Validate(); err != nil {
			return domain.AccessPolicySnapshot{}, err
		}
		normalized = append(normalized, rule)
	}
	snapshot := domain.AccessPolicySnapshot{
		SnapshotID:    fmt.Sprintf("%s-snap-%d", publicationID, version),
		PublicationID: publicationID,
		Version:       version,
		Rules:         normalized,
		CreatedBy:     actor,
		CreatedAt:     now,
	}
	r.appendSnapshotLocked(publicationID, snapshot)
	r.nextVersion[publicationID] = version
	return snapshot, nil
}

func (r *TargetAccessRepo) CurrentSnapshot(_ context.Context, publicationID string) (domain.AccessPolicySnapshot, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	history := r.snapshots[publicationID]
	if len(history) == 0 {
		return domain.AccessPolicySnapshot{}, domain.ErrNotFound
	}
	return history[len(history)-1], nil
}

func (r *TargetAccessRepo) CurrentRules(_ context.Context, publicationID string) ([]domain.InitiatorRule, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	history := r.snapshots[publicationID]
	if len(history) == 0 {
		return []domain.InitiatorRule{}, nil
	}
	rules := history[len(history)-1].Rules
	out := make([]domain.InitiatorRule, len(rules))
	copy(out, rules)
	return out, nil
}

func (r *TargetAccessRepo) Rollback(_ context.Context, publicationID, actor string) (domain.AccessPolicySnapshot, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	history := r.snapshots[publicationID]
	if len(history) <= 1 {
		if len(history) == 0 {
			return domain.AccessPolicySnapshot{}, true, domain.ErrNotFound
		}
		return history[len(history)-1], true, nil
	}

	rules := history[len(history)-2].Rules
	version := r.nextVersion[publicationID] + 1
	now := time.Now().UTC()
	restored := make([]domain.InitiatorRule, 0, len(rules))
	for i, rule := range rules {
		rule.RuleID = fmt.Sprintf("%s-rule-%d-v%d", publicationID, i+1, version)
		rule.CreatedAt = now
		restored = append(restored, rule)
	}
	snapshot := domain.AccessPolicySnapshot{
		SnapshotID:    fmt.Sprintf("%s-snap-%d", publicationID, version),
		PublicationID: publicationID,
		Version:       version,
		Rules:         restored,
		CreatedBy:     actor,
		CreatedAt:     now,
	}
	r.appendSnapshotLocked(publicationID, snapshot)
	r.nextVersion[publicationID] = version
	return snapshot, false, nil
}

func (r *TargetAccessRepo) SnapshotCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	count := 0
	for _, history := range r.snapshots {
		count += len(history)
	}
	return count
}

func (r *TargetAccessRepo) appendSnapshotLocked(publicationID string, snapshot domain.AccessPolicySnapshot) {
	history := append(r.snapshots[publicationID], snapshot)
	if len(history) > maxSnapshotHistory {
		history = history[len(history)-maxSnapshotHistory:]
	}
	r.snapshots[publicationID] = history
}
