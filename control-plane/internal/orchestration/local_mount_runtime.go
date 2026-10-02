package orchestration

import (
	"context"
	"errors"

	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
)

type LocalMountLoopbackHelper interface {
	Probe(context.Context) (bool, domain.LocalMountReasonCode, error)
	ListOwned(context.Context) ([]LocalLoopbackOwner, error)
	Ensure(context.Context, domain.LocalLoopbackLibraryMapping, []domain.LocalLoopbackDeviceMapping) ([]LocalLoopbackDeviceObservation, error)
	Remove(context.Context, domain.LocalLoopbackLibraryMapping, []domain.LocalLoopbackDeviceMapping) ([]LocalLoopbackDeviceObservation, error)
}

type LocalMountBackendRuntime interface {
	EnsureLocalMountBackend(context.Context, domain.VTLDeviceDescriptor) error
	ReleaseLocalMountBackend(context.Context, domain.LocalLoopbackDeviceMapping) error
}

type localMountIdentityBackend interface {
	ResolveLocalMountIdentity(context.Context, domain.VTLDeviceDescriptor, *domain.TargetPublication) (string, error)
}

type localMountDeviceLocker interface {
	WithLocalMountDeviceLocks(context.Context, []string, func() error) error
}

type ComposedLocalMountRuntime struct {
	helper  LocalMountLoopbackHelper
	backend LocalMountBackendRuntime
}

func NewComposedLocalMountRuntime(helper LocalMountLoopbackHelper, backend LocalMountBackendRuntime) *ComposedLocalMountRuntime {
	return &ComposedLocalMountRuntime{helper: helper, backend: backend}
}

func (r *ComposedLocalMountRuntime) Probe(ctx context.Context) (bool, domain.LocalMountReasonCode, error) {
	if r == nil || r.helper == nil {
		return false, domain.LocalMountReasonLoopbackUnavailable, ErrLocalLoopbackHelperUnavailable
	}
	return r.helper.Probe(ctx)
}

func (r *ComposedLocalMountRuntime) ListOwned(ctx context.Context) ([]LocalLoopbackOwner, error) {
	if r == nil || r.helper == nil {
		return nil, ErrLocalLoopbackHelperUnavailable
	}
	return r.helper.ListOwned(ctx)
}

func (r *ComposedLocalMountRuntime) Ensure(ctx context.Context, library domain.LocalLoopbackLibraryMapping, mappings []domain.LocalLoopbackDeviceMapping, descriptors []domain.VTLDeviceDescriptor) ([]LocalLoopbackDeviceObservation, error) {
	if r == nil || r.helper == nil || r.backend == nil {
		return nil, ErrLocalLoopbackHelperUnavailable
	}
	descriptorByKey := make(map[string]domain.VTLDeviceDescriptor, len(descriptors))
	backendErrors := make(map[string]error)
	for _, descriptor := range descriptors {
		descriptorByKey[descriptor.DeviceKey] = descriptor
	}
	deviceKeys := make([]string, 0, len(mappings))
	for _, mapping := range mappings {
		deviceKeys = append(deviceKeys, mapping.DeviceKey)
	}
	var observations []LocalLoopbackDeviceObservation
	operation := func() error {
		for _, mapping := range mappings {
			descriptor, ok := descriptorByKey[mapping.DeviceKey]
			if !ok {
				backendErrors[mapping.DeviceKey] = domain.ErrNotFound
				continue
			}
			if !descriptor.BackendReady {
				backendErrors[mapping.DeviceKey] = LocalLoopbackHelperError{ReasonCode: string(descriptor.ReadinessReason)}
				continue
			}
			if err := r.backend.EnsureLocalMountBackend(ctx, descriptor); err != nil {
				backendErrors[mapping.DeviceKey] = err
			}
		}
		var err error
		observations, err = r.helper.Ensure(ctx, library, mappings)
		return err
	}
	var err error
	if locker, ok := r.backend.(localMountDeviceLocker); ok {
		err = locker.WithLocalMountDeviceLocks(ctx, deviceKeys, operation)
	} else {
		err = operation()
	}
	if err != nil {
		return failedLocalMountObservations(mappings, err), err
	}
	for i := range observations {
		if backendErr := backendErrors[observations[i].DeviceKey]; backendErr != nil {
			observations[i].State = domain.LocalMountDeviceStateFailed
			observations[i].ObservedPaths = nil
			observations[i].ReasonCode = localMountReasonForError(backendErr)
		}
	}
	return observations, nil
}

func (r *ComposedLocalMountRuntime) Remove(ctx context.Context, library domain.LocalLoopbackLibraryMapping, mappings []domain.LocalLoopbackDeviceMapping) ([]LocalLoopbackDeviceObservation, error) {
	if r == nil || r.helper == nil {
		return nil, ErrLocalLoopbackHelperUnavailable
	}
	operation := func() error { return nil }
	var observations []LocalLoopbackDeviceObservation
	operation = func() error {
		var err error
		observations, err = r.helper.Remove(ctx, library, mappings)
		return err
	}
	if locker, ok := r.backend.(localMountDeviceLocker); ok {
		deviceKeys := make([]string, 0, len(mappings))
		for _, mapping := range mappings {
			deviceKeys = append(deviceKeys, mapping.DeviceKey)
		}
		if err := locker.WithLocalMountDeviceLocks(ctx, deviceKeys, operation); err != nil {
			return observations, err
		}
		return observations, nil
	}
	if err := operation(); err != nil {
		return observations, err
	}
	return observations, nil
}

func (r *ComposedLocalMountRuntime) ReleaseBackend(ctx context.Context, mapping domain.LocalLoopbackDeviceMapping) error {
	if r == nil || r.backend == nil {
		return ErrLocalLoopbackHelperUnavailable
	}
	return r.backend.ReleaseLocalMountBackend(ctx, mapping)
}

func (r *ComposedLocalMountRuntime) ResolveLocalMountIdentity(ctx context.Context, descriptor domain.VTLDeviceDescriptor, publication *domain.TargetPublication) (string, error) {
	if r == nil || r.backend == nil {
		return "", ErrLocalLoopbackHelperUnavailable
	}
	if resolver, ok := r.backend.(localMountIdentityBackend); ok {
		return resolver.ResolveLocalMountIdentity(ctx, descriptor, publication)
	}
	return descriptor.IdentityRef, nil
}

func failedLocalMountObservations(mappings []domain.LocalLoopbackDeviceMapping, err error) []LocalLoopbackDeviceObservation {
	observations := make([]LocalLoopbackDeviceObservation, 0, len(mappings))
	for _, mapping := range mappings {
		observations = append(observations, LocalLoopbackDeviceObservation{
			DeviceKey:  mapping.DeviceKey,
			State:      domain.LocalMountDeviceStateFailed,
			ReasonCode: localMountReasonForError(err),
		})
	}
	return observations
}

func localMountReasonForError(err error) domain.LocalMountReasonCode {
	if err == nil {
		return ""
	}
	var helperErr LocalLoopbackHelperError
	if errors.As(err, &helperErr) {
		reason := domain.LocalMountReasonCode(helperErr.ReasonCode)
		if knownLocalMountReason(reason) {
			return reason
		}
	}
	if errors.Is(err, ErrDeviceBusy) {
		return domain.LocalMountReasonDeviceBusy
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return domain.LocalMountReasonOperationTimeout
	}
	return domain.LocalMountReasonBackendNotReady
}
