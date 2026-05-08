package memory

import (
	"context"
	"sync"

	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
)

type RetentionPolicyRepo struct {
	mu       sync.RWMutex
	policies map[string]domain.RetentionPolicy
}

func NewRetentionPolicyRepo() *RetentionPolicyRepo {
	return &RetentionPolicyRepo{policies: make(map[string]domain.RetentionPolicy)}
}

func (r *RetentionPolicyRepo) Save(_ context.Context, p domain.RetentionPolicy) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.policies[p.RetentionID] = p
	return nil
}
