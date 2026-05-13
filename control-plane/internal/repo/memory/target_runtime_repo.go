package memory

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
)

const maxValidationMediaEntries = 1000
const maxValidationMediaAge = time.Hour

type TargetRuntimeRepo struct {
	mu                     sync.RWMutex
	publications           map[string]*domain.TargetPublication
	validationsByID        map[string]*domain.ValidationRun
	validationsByPub       map[string][]*domain.ValidationRun
	validationMedia        map[string][]byte
	validationMediaWritten map[string]time.Time
	validationMediaOrder   []string
}

func NewTargetRuntimeRepo() *TargetRuntimeRepo {
	return &TargetRuntimeRepo{
		publications:           make(map[string]*domain.TargetPublication),
		validationsByID:        make(map[string]*domain.ValidationRun),
		validationsByPub:       make(map[string][]*domain.ValidationRun),
		validationMedia:        make(map[string][]byte),
		validationMediaWritten: make(map[string]time.Time),
		validationMediaOrder:   make([]string, 0, maxValidationMediaEntries),
	}
}

func (r *TargetRuntimeRepo) SavePublication(_ context.Context, p *domain.TargetPublication) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.publications[p.PublicationID] = clonePublication(p)
	return nil
}

func (r *TargetRuntimeRepo) SavePublicationIfIQNAvailable(_ context.Context, p *domain.TargetPublication) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, existing := range r.publications {
		if existing.TargetIQN == p.TargetIQN &&
			existing.PublicationID != p.PublicationID &&
			(existing.State == domain.PublicationCreating || existing.State == domain.PublicationReady) {
			return domain.ErrConflict
		}
	}
	r.publications[p.PublicationID] = clonePublication(p)
	return nil
}

func (r *TargetRuntimeRepo) FindPublication(_ context.Context, publicationID string) (*domain.TargetPublication, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.publications[publicationID]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return clonePublication(p), nil
}

func (r *TargetRuntimeRepo) ListPublications(_ context.Context) []*domain.TargetPublication {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*domain.TargetPublication, 0, len(r.publications))
	for _, p := range r.publications {
		out = append(out, clonePublication(p))
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].PublicationID < out[j].PublicationID
	})
	return out
}

func (r *TargetRuntimeRepo) ListDiscoverablePublications(_ context.Context) []*domain.TargetPublication {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*domain.TargetPublication, 0, len(r.publications))
	for _, p := range r.publications {
		if p.State != domain.PublicationReady {
			continue
		}
		if p.TargetIQN == "" || p.Portal == "" {
			continue
		}
		out = append(out, clonePublication(p))
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].PublicationID < out[j].PublicationID
	})
	return out
}

func (r *TargetRuntimeRepo) FindPublicationByIQN(_ context.Context, iqn string) (*domain.TargetPublication, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, p := range r.publications {
		if p.TargetIQN == iqn &&
			(p.State == domain.PublicationCreating || p.State == domain.PublicationReady) {
			return clonePublication(p), true
		}
	}
	return nil, false
}

func (r *TargetRuntimeRepo) SaveValidationRun(_ context.Context, run *domain.ValidationRun) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := cloneValidationRun(run)
	r.validationsByID[run.ValidationID] = cp
	r.validationsByPub[run.PublicationID] = append(r.validationsByPub[run.PublicationID], cp)
	return nil
}

func (r *TargetRuntimeRepo) WriteValidationMedia(_ context.Context, publicationID string, payload []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.publications[publicationID]; !ok {
		return domain.ErrNotFound
	}
	cp := make([]byte, len(payload))
	copy(cp, payload)
	if _, exists := r.validationMedia[publicationID]; !exists {
		r.validationMediaOrder = append(r.validationMediaOrder, publicationID)
	}
	r.validationMedia[publicationID] = cp
	now := time.Now().UTC()
	r.validationMediaWritten[publicationID] = now
	r.evictOldValidationMediaLocked(now, maxValidationMediaAge)
	r.evictValidationMediaOverflowLocked()
	return nil
}

func (r *TargetRuntimeRepo) ReadValidationMedia(_ context.Context, publicationID string) ([]byte, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if _, ok := r.publications[publicationID]; !ok {
		return nil, domain.ErrNotFound
	}
	payload := r.validationMedia[publicationID]
	cp := make([]byte, len(payload))
	copy(cp, payload)
	return cp, nil
}

func (r *TargetRuntimeRepo) ListValidationRuns(_ context.Context, publicationID string) []*domain.ValidationRun {
	r.mu.RLock()
	defer r.mu.RUnlock()
	runs := r.validationsByPub[publicationID]
	out := make([]*domain.ValidationRun, len(runs))
	for i := range runs {
		out[i] = cloneValidationRun(runs[i])
	}
	return out
}

func (r *TargetRuntimeRepo) EvictOldValidationMedia(now time.Time, maxAge time.Duration) {
	if maxAge <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evictOldValidationMediaLocked(now, maxAge)
}

func (r *TargetRuntimeRepo) evictOldValidationMediaLocked(now time.Time, maxAge time.Duration) {
	cutoff := now.Add(-maxAge)
	keptOrder := make([]string, 0, len(r.validationMediaOrder))
	for _, publicationID := range r.validationMediaOrder {
		writtenAt, ok := r.validationMediaWritten[publicationID]
		if !ok || writtenAt.Before(cutoff) {
			delete(r.validationMedia, publicationID)
			delete(r.validationMediaWritten, publicationID)
			continue
		}
		keptOrder = append(keptOrder, publicationID)
	}
	r.validationMediaOrder = keptOrder
}

func (r *TargetRuntimeRepo) evictValidationMediaOverflowLocked() {
	for len(r.validationMediaOrder) > maxValidationMediaEntries {
		evictID := r.validationMediaOrder[0]
		r.validationMediaOrder = r.validationMediaOrder[1:]
		delete(r.validationMedia, evictID)
		delete(r.validationMediaWritten, evictID)
	}
}

func clonePublication(in *domain.TargetPublication) *domain.TargetPublication {
	if in == nil {
		return nil
	}
	cp := *in
	if in.ConnectedHosts != nil {
		hosts := *in.ConnectedHosts
		hosts.Initiators = append([]string(nil), in.ConnectedHosts.Initiators...)
		cp.ConnectedHosts = &hosts
	}
	return &cp
}

func cloneValidationRun(in *domain.ValidationRun) *domain.ValidationRun {
	if in == nil {
		return nil
	}
	cp := *in
	return &cp
}
