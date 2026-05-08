package memory

import (
	"context"
	"sync"

	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
)

type AccessPolicyRepo struct {
	mu       sync.RWMutex
	policies map[string]domain.TargetAccessPolicy
}

func NewAccessPolicyRepo() *AccessPolicyRepo {
	return &AccessPolicyRepo{policies: make(map[string]domain.TargetAccessPolicy)}
}

func (r *AccessPolicyRepo) Save(_ context.Context, p domain.TargetAccessPolicy) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.policies[p.PolicyID] = p
	return nil
}

func (r *AccessPolicyRepo) List(_ context.Context) ([]domain.TargetAccessPolicy, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]domain.TargetAccessPolicy, 0, len(r.policies))
	for _, p := range r.policies {
		out = append(out, p)
	}
	return out, nil
}
