package orchestration

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestDeviceRuntimeSharesBackendAndReleasesOnlyRequestedConsumer(t *testing.T) {
	backend := newFakeSharedDeviceBackend()
	service := NewDeviceRuntimeService(backend)
	ctx := context.Background()
	key := "drive:drive-a"

	if err := service.Acquire(ctx, key, DeviceConsumerISCSI); err != nil {
		t.Fatalf("acquire iSCSI consumer: %v", err)
	}
	if err := service.Acquire(ctx, key, DeviceConsumerLoopback); err != nil {
		t.Fatalf("acquire loopback consumer: %v", err)
	}
	if backend.ensureCount(key) != 1 {
		t.Fatalf("expected a single canonical backend, ensure count=%d", backend.ensureCount(key))
	}

	if err := service.Release(ctx, key, DeviceConsumerISCSI); err != nil {
		t.Fatalf("release iSCSI consumer: %v", err)
	}
	if got := backend.consumerNames(key); len(got) != 1 || got[0] != DeviceConsumerLoopback {
		t.Fatalf("network release removed the local consumer: %v", got)
	}
	if backend.releaseCount(key) != 0 {
		t.Fatalf("shared backend released while loopback consumer remains")
	}
}

func TestDeviceRuntimeRetainsBackendWhenConsumerOwnershipIsUnknown(t *testing.T) {
	backend := newFakeSharedDeviceBackend()
	service := NewDeviceRuntimeService(backend)
	ctx := context.Background()
	key := "drive:drive-a"
	if err := service.Acquire(ctx, key, DeviceConsumerLoopback); err != nil {
		t.Fatalf("acquire loopback consumer: %v", err)
	}
	backend.setObservationError(key, errors.New("ownership observation unavailable"))

	if err := service.Release(ctx, key, DeviceConsumerLoopback); err == nil {
		t.Fatal("expected unknown ownership to be reported")
	}
	if backend.releaseCount(key) != 0 {
		t.Fatal("released backend after consumer ownership became unknown")
	}
}

func TestDeviceRuntimeDoesNotReleaseUndrainedBackend(t *testing.T) {
	backend := newFakeSharedDeviceBackend()
	service := NewDeviceRuntimeService(backend)
	ctx := context.Background()
	key := "drive:drive-a"
	if err := service.Acquire(ctx, key, DeviceConsumerLoopback); err != nil {
		t.Fatalf("acquire loopback consumer: %v", err)
	}
	backend.setDrained(key, false)

	err := service.Release(ctx, key, DeviceConsumerLoopback)
	if !errors.Is(err, ErrDeviceBusy) {
		t.Fatalf("expected ErrDeviceBusy, got %v", err)
	}
	if backend.releaseCount(key) != 0 {
		t.Fatal("released a backend before commands drained")
	}
}

func TestDeviceRuntimeReleasesLastConsumerOnlyAfterDrain(t *testing.T) {
	backend := newFakeSharedDeviceBackend()
	service := NewDeviceRuntimeService(backend)
	ctx := context.Background()
	key := "drive:drive-a"
	if err := service.Acquire(ctx, key, DeviceConsumerLoopback); err != nil {
		t.Fatalf("acquire loopback consumer: %v", err)
	}
	if err := service.Release(ctx, key, DeviceConsumerLoopback); err != nil {
		t.Fatalf("release drained final consumer: %v", err)
	}
	if backend.releaseCount(key) != 1 {
		t.Fatalf("expected exactly one backend release, got %d", backend.releaseCount(key))
	}
}

func TestDeviceRuntimeSerializesSameDeviceWithoutBlockingOtherDevices(t *testing.T) {
	backend := newFakeSharedDeviceBackend()
	backend.delay = 20 * time.Millisecond
	service := NewDeviceRuntimeService(backend)
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := service.Acquire(ctx, "drive:drive-a", DeviceConsumerLoopback); err != nil {
				t.Errorf("acquire same device: %v", err)
			}
		}()
	}
	if err := service.Acquire(ctx, "drive:drive-b", DeviceConsumerLoopback); err != nil {
		t.Fatalf("acquire independent device: %v", err)
	}
	wg.Wait()
	if backend.ensureCount("drive:drive-a") != 1 || backend.ensureCount("drive:drive-b") != 1 {
		t.Fatalf("expected one backend per device, got a=%d b=%d", backend.ensureCount("drive:drive-a"), backend.ensureCount("drive:drive-b"))
	}
	if backend.maxActiveCount() < 2 {
		t.Fatal("independent devices were serialized behind one global lifecycle lock")
	}
}

type fakeSharedDeviceBackend struct {
	mu                sync.Mutex
	ensures           map[string]int
	backends          map[string]bool
	releases          map[string]int
	consumers         map[string]map[string]bool
	drained           map[string]bool
	observationErrors map[string]error
	delay             time.Duration
	active            int
	maxActive         int
}

func newFakeSharedDeviceBackend() *fakeSharedDeviceBackend {
	return &fakeSharedDeviceBackend{
		ensures:           make(map[string]int),
		backends:          make(map[string]bool),
		releases:          make(map[string]int),
		consumers:         make(map[string]map[string]bool),
		drained:           make(map[string]bool),
		observationErrors: make(map[string]error),
	}
}

func (b *fakeSharedDeviceBackend) EnsureBackend(_ context.Context, key string) error {
	b.mu.Lock()
	if !b.backends[key] {
		b.backends[key] = true
		b.ensures[key]++
	}
	if b.consumers[key] == nil {
		b.consumers[key] = make(map[string]bool)
	}
	if _, exists := b.drained[key]; !exists {
		b.drained[key] = true
	}
	b.active++
	if b.active > b.maxActive {
		b.maxActive = b.active
	}
	b.mu.Unlock()
	if b.delay != 0 {
		time.Sleep(b.delay)
	}
	b.mu.Lock()
	b.active--
	b.mu.Unlock()
	return nil
}

func (b *fakeSharedDeviceBackend) AttachConsumer(_ context.Context, key, consumer string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.consumers[key] == nil {
		b.consumers[key] = make(map[string]bool)
	}
	b.consumers[key][consumer] = true
	return nil
}

func (b *fakeSharedDeviceBackend) DetachConsumer(_ context.Context, key, consumer string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.consumers[key], consumer)
	return nil
}

func (b *fakeSharedDeviceBackend) Consumers(_ context.Context, key string) (map[string]bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.observationErrors[key]; err != nil {
		return nil, err
	}
	result := make(map[string]bool, len(b.consumers[key]))
	for consumer, present := range b.consumers[key] {
		result[consumer] = present
	}
	return result, nil
}

func (b *fakeSharedDeviceBackend) Drain(_ context.Context, key string) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.drained[key], nil
}

func (b *fakeSharedDeviceBackend) ReleaseBackend(_ context.Context, key string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.releases[key]++
	return nil
}

func (b *fakeSharedDeviceBackend) ensureCount(key string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.ensures[key]
}

func (b *fakeSharedDeviceBackend) releaseCount(key string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.releases[key]
}

func (b *fakeSharedDeviceBackend) consumerNames(key string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	result := make([]string, 0, len(b.consumers[key]))
	for consumer := range b.consumers[key] {
		result = append(result, consumer)
	}
	return result
}

func (b *fakeSharedDeviceBackend) setObservationError(key string, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.observationErrors[key] = err
}

func (b *fakeSharedDeviceBackend) setDrained(key string, drained bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.drained[key] = drained
}

func (b *fakeSharedDeviceBackend) maxActiveCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.maxActive
}
