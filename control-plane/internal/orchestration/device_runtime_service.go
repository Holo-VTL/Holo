package orchestration

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
)

const (
	DeviceConsumerISCSI    = "iscsi"
	DeviceConsumerLoopback = "loopback"
)

var ErrDeviceBusy = errors.New("device backend is busy")

type SharedDeviceBackend interface {
	EnsureBackend(context.Context, string) error
	AttachConsumer(context.Context, string, string) error
	DetachConsumer(context.Context, string, string) error
	Consumers(context.Context, string) (map[string]bool, error)
	Drain(context.Context, string) (bool, error)
	ReleaseBackend(context.Context, string) error
}

type DeviceRuntimeService struct {
	backend SharedDeviceBackend
	mu      sync.Mutex
	locks   map[string]*sync.Mutex
}

func NewDeviceRuntimeService(backend SharedDeviceBackend) *DeviceRuntimeService {
	return &DeviceRuntimeService{backend: backend, locks: make(map[string]*sync.Mutex)}
}

func (s *DeviceRuntimeService) Acquire(ctx context.Context, deviceKey, consumer string) error {
	if s == nil || s.backend == nil || !validLocalDeviceKey(deviceKey) || !validDeviceConsumer(consumer) {
		return domain.ErrInvalidInput
	}
	lock := s.deviceLock(deviceKey)
	lock.Lock()
	defer lock.Unlock()
	if err := s.backend.EnsureBackend(ctx, deviceKey); err != nil {
		return fmt.Errorf("ensure shared device backend: %w", err)
	}
	if err := s.backend.AttachConsumer(ctx, deviceKey, consumer); err != nil {
		return fmt.Errorf("attach shared device consumer: %w", err)
	}
	return nil
}

func (s *DeviceRuntimeService) Release(ctx context.Context, deviceKey, consumer string) error {
	if s == nil || s.backend == nil || !validLocalDeviceKey(deviceKey) || !validDeviceConsumer(consumer) {
		return domain.ErrInvalidInput
	}
	lock := s.deviceLock(deviceKey)
	lock.Lock()
	defer lock.Unlock()
	if err := s.backend.DetachConsumer(ctx, deviceKey, consumer); err != nil {
		return fmt.Errorf("detach shared device consumer: %w", err)
	}
	consumers, err := s.backend.Consumers(ctx, deviceKey)
	if err != nil {
		return fmt.Errorf("observe shared device consumers: %w", err)
	}
	if hasDeviceConsumers(consumers) {
		return nil
	}
	drained, err := s.backend.Drain(ctx, deviceKey)
	if err != nil {
		return fmt.Errorf("drain shared device backend: %w", err)
	}
	if !drained {
		return ErrDeviceBusy
	}
	consumers, err = s.backend.Consumers(ctx, deviceKey)
	if err != nil {
		return fmt.Errorf("recheck shared device consumers: %w", err)
	}
	if hasDeviceConsumers(consumers) {
		return nil
	}
	if err := s.backend.ReleaseBackend(ctx, deviceKey); err != nil {
		return fmt.Errorf("release shared device backend: %w", err)
	}
	return nil
}

func (s *DeviceRuntimeService) deviceLock(deviceKey string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	lock := s.locks[deviceKey]
	if lock == nil {
		lock = &sync.Mutex{}
		s.locks[deviceKey] = lock
	}
	return lock
}

func validLocalDeviceKey(value string) bool {
	role, id, ok := strings.Cut(value, ":")
	if !ok || domain.ValidateManagementID(id) != nil {
		return false
	}
	return role == string(domain.LocalDeviceKindChanger) || role == string(domain.LocalDeviceKindDrive)
}

func validDeviceConsumer(value string) bool {
	return value == DeviceConsumerISCSI || value == DeviceConsumerLoopback
}

func hasDeviceConsumers(consumers map[string]bool) bool {
	for _, present := range consumers {
		if present {
			return true
		}
	}
	return false
}
