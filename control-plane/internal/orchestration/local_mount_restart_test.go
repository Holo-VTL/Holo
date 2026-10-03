package orchestration

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Holo-VTL/Holo/control-plane/internal/audit"
	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
	"github.com/Holo-VTL/Holo/control-plane/internal/repo/memory"
)

type restartLocalLifecycle interface {
	PrepareRestart(context.Context, string) error
	Stop(context.Context, string) error
}

type restartOrderMount struct {
	mounted bool
	calls   []string
	err     error
}

func (m *restartOrderMount) Sync(context.Context, string) (LocalMountStatus, error) {
	return LocalMountStatus{}, nil
}
func (m *restartOrderMount) SyncAsync(string) {
	m.calls = append(m.calls, "mount")
	m.mounted = true
}
func (m *restartOrderMount) PrepareRestart(context.Context, string) error {
	m.calls = append(m.calls, "prepare")
	if m.err == nil {
		m.mounted = false
	}
	return m.err
}
func (m *restartOrderMount) Stop(context.Context, string) error {
	m.calls = append(m.calls, "stop")
	if m.err == nil {
		m.mounted = false
	}
	return m.err
}

type restartOrderAdapter struct{ mount *restartOrderMount }

func (a restartOrderAdapter) ListSessions(context.Context) ([]TargetSession, error) {
	return nil, nil
}

func (a restartOrderAdapter) Publish(context.Context, *domain.TargetPublication) (string, error) {
	a.mount.calls = append(a.mount.calls, "publish")
	if a.mount.mounted {
		return "", ErrDeviceBusy
	}
	return "10.10.1.191:3260", nil
}
func (a restartOrderAdapter) Unpublish(context.Context, *domain.TargetPublication) error {
	a.mount.calls = append(a.mount.calls, "unpublish")
	if a.mount.mounted {
		return ErrDeviceBusy
	}
	return nil
}

func restartOrderService(t *testing.T, mount *restartOrderMount) (*TargetRuntimeService, *memory.TargetRuntimeRepo) {
	t.Helper()
	repository := memory.NewTargetRuntimeRepo()
	publication := tcmuTestPublication()
	if err := publication.MarkReady("10.10.1.191:3260"); err != nil {
		t.Fatal(err)
	}
	if err := repository.SavePublication(context.Background(), publication); err != nil {
		t.Fatal(err)
	}
	service := newTargetRuntimeServiceWithAdapter(memory.NewCoreResourcesRepo(), repository, audit.NewMemoryWriter(), nil, TargetRuntimeConfig{Mode: "tcmu"}, restartOrderAdapter{mount})
	service.SetLocalMountSynchronizer(mount)
	return service, repository
}

func TestLocalMountRestartDetachesBeforeRestoringNetworkBackstores(t *testing.T) {
	mount := &restartOrderMount{mounted: true}
	service, repository := restartOrderService(t, mount)
	if err := service.RestoreReadyPublications(context.Background()); err != nil {
		t.Fatalf("restore collided with retained local LUNs: %v", err)
	}
	if !reflect.DeepEqual(mount.calls, []string{"prepare", "unpublish", "publish", "mount"}) {
		t.Fatalf("unsafe restore ordering: %v", mount.calls)
	}
	publication, err := repository.FindPublication(context.Background(), tcmuTestPublication().PublicationID)
	if err != nil || publication.State != domain.PublicationReady || publication.LastError != "" || !mount.mounted {
		t.Fatalf("existing publication/mount did not recover: %+v err=%v", publication, err)
	}
}

func TestLocalMountRestartStopsBeforeNetworkShutdown(t *testing.T) {
	mount := &restartOrderMount{mounted: true}
	service, repository := restartOrderService(t, mount)
	if err := service.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown left local references attached: %v", err)
	}
	if !reflect.DeepEqual(mount.calls, []string{"stop", "unpublish"}) {
		t.Fatalf("unsafe shutdown ordering: %v", mount.calls)
	}
	publication, err := repository.FindPublication(context.Background(), tcmuTestPublication().PublicationID)
	if err != nil || publication.State != domain.PublicationReady {
		t.Fatalf("shutdown changed publication intent: %+v err=%v", publication, err)
	}
}

func TestLocalMountRestartCleanupFailureLeavesReadyPublicationsUntouched(t *testing.T) {
	for _, action := range []string{"start", "stop"} {
		t.Run(action, func(t *testing.T) {
			mount := &restartOrderMount{mounted: true, err: ErrDeviceBusy}
			service, repository := restartOrderService(t, mount)
			var err error
			if action == "start" {
				err = service.RestoreReadyPublications(context.Background())
			} else {
				err = service.Shutdown(context.Background())
			}
			if !errors.Is(err, ErrDeviceBusy) || len(mount.calls) != 1 {
				t.Fatalf("cleanup error did not stop shared-backend mutations: calls=%v err=%v", mount.calls, err)
			}
			publication, err := repository.FindPublication(context.Background(), tcmuTestPublication().PublicationID)
			if err != nil || publication.State != domain.PublicationReady || publication.LastError != "" {
				t.Fatalf("cleanup failure corrupted ready publication: %+v err=%v", publication, err)
			}
		})
	}
}

func requireRestartLifecycle(t *testing.T, service *LocalMountService) restartLocalLifecycle {
	t.Helper()
	lifecycle, ok := any(service).(restartLocalLifecycle)
	if !ok {
		t.Fatal("local mounting has no process restart lifecycle")
	}
	return lifecycle
}

func TestLocalMountRestartCleanupRetainsEnabledIntentAndStableMappings(t *testing.T) {
	ctx := context.Background()
	settings := memory.NewLocalMountRepo()
	if err := settings.SetEnabled(ctx, true); err != nil {
		t.Fatal(err)
	}
	runtime := &fakeLocalMountRuntime{available: true}
	service := NewLocalMountService(settings, memory.NewTargetRuntimeRepo(), audit.NewMemoryWriter(), TargetRuntimeConfig{})
	service.SetRuntime(newLocalMountServiceResources(t), nil, settings, runtime)
	lifecycle := requireRestartLifecycle(t, service)
	if _, err := service.Sync(ctx, "initial"); err != nil {
		t.Fatal(err)
	}
	before, err := settings.ListDeviceMappings(ctx, "lib-a")
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := lifecycle.PrepareRestart(ctx, "system"); err != nil {
			t.Fatal(err)
		}
		enabled, err := settings.Enabled(ctx)
		if err != nil || !enabled || len(runtime.owners) != 0 {
			t.Fatalf("restart cleanup changed intent or left local mappings: enabled=%v err=%v owners=%v", enabled, err, runtime.owners)
		}
		after, err := settings.ListDeviceMappings(ctx, "lib-a")
		if err != nil || len(after) != len(before) {
			t.Fatalf("stable mappings were removed: %v err=%v", after, err)
		}
		for i := range after {
			want := before[i]
			want.State = domain.LocalMappingStateInactive
			if after[i] != want {
				t.Fatalf("restart changed device identity: got=%+v want=%+v", after[i], want)
			}
		}
	}
	if _, err := service.Sync(ctx, "restart"); err != nil {
		t.Fatal(err)
	}
	status, err := service.Status(ctx)
	if err != nil || !status.Enabled || status.State != domain.LocalMountStateConnected || status.ConnectedDeviceCount != 3 {
		t.Fatalf("preserved intent did not remount: %+v err=%v", status, err)
	}
}

func TestLocalMountRestartBusyCleanupRetainsOwnershipAndIntent(t *testing.T) {
	ctx := context.Background()
	settings := memory.NewLocalMountRepo()
	if err := settings.SetEnabled(ctx, true); err != nil {
		t.Fatal(err)
	}
	runtime := &fakeLocalMountRuntime{available: true}
	service := NewLocalMountService(settings, memory.NewTargetRuntimeRepo(), audit.NewMemoryWriter(), TargetRuntimeConfig{})
	service.SetRuntime(newLocalMountServiceResources(t), nil, settings, runtime)
	lifecycle := requireRestartLifecycle(t, service)
	if _, err := service.Sync(ctx, "initial"); err != nil {
		t.Fatal(err)
	}
	runtime.removeErr = LocalLoopbackHelperError{ReasonCode: "device_busy"}
	if err := lifecycle.PrepareRestart(ctx, "system"); err == nil {
		t.Fatal("busy restart cleanup reported success")
	}
	enabled, err := settings.Enabled(ctx)
	if err != nil || !enabled || len(runtime.owners) != 1 || len(runtime.released) != 0 {
		t.Fatalf("busy cleanup altered intent/ownership or released backend: enabled=%v err=%v owners=%v released=%v", enabled, err, runtime.owners, runtime.released)
	}
	status, err := service.Status(ctx)
	if err != nil || !status.Enabled || status.State != domain.LocalMountStateFailed || status.LastError != "device_busy" {
		t.Fatalf("busy cleanup did not preserve failure feedback: %+v err=%v", status, err)
	}
}

type cancelRestartRuntime struct {
	*fakeLocalMountRuntime
	entered  chan struct{}
	attempts atomic.Int32
}

func (r *cancelRestartRuntime) Ensure(ctx context.Context, _ domain.LocalLoopbackLibraryMapping, _ []domain.LocalLoopbackDeviceMapping, _ []domain.VTLDeviceDescriptor) ([]LocalLoopbackDeviceObservation, error) {
	r.attempts.Add(1)
	close(r.entered)
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestLocalMountRestartStopCancelsCoordinatorAndPreventsRemount(t *testing.T) {
	settings := memory.NewLocalMountRepo()
	runtime := &cancelRestartRuntime{fakeLocalMountRuntime: &fakeLocalMountRuntime{available: true}, entered: make(chan struct{})}
	service := NewLocalMountService(settings, memory.NewTargetRuntimeRepo(), audit.NewMemoryWriter(), TargetRuntimeConfig{})
	service.SetRuntime(newLocalMountServiceResources(t), nil, settings, runtime)
	lifecycle := requireRestartLifecycle(t, service)
	if _, err := service.SetEnabled(context.Background(), true, "initial"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-runtime.entered:
	case <-time.After(time.Second):
		t.Fatal("coordinator never started")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := lifecycle.Stop(ctx, "shutdown"); err != nil {
		t.Fatalf("coordinator was not canceled before cleanup: %v", err)
	}
	service.SyncAsync("late-publication")
	waitLocalMountIdle(t, service)
	if runtime.attempts.Load() != 1 {
		t.Fatal("coordinator remounted after shutdown cleanup")
	}
	if _, err := service.Sync(ctx, "late-sync"); !errors.Is(err, domain.ErrInvalidState) {
		t.Fatalf("stopped service accepted direct coordination: %v", err)
	}
	if _, err := service.SetEnabled(context.Background(), false, "late-request"); !errors.Is(err, domain.ErrInvalidState) {
		t.Fatalf("stopped service accepted another setting change: %v", err)
	}
	enabled, err := settings.Enabled(context.Background())
	if err != nil || !enabled {
		t.Fatalf("shutdown erased persistent enabled intent: enabled=%v err=%v", enabled, err)
	}
}

type unknownRestartRuntime struct {
	*fakeLocalMountRuntime
	failOn int
	lists  int
}

func (r *unknownRestartRuntime) ListOwned(ctx context.Context) ([]LocalLoopbackOwner, error) {
	r.lists++
	if r.lists == r.failOn {
		return nil, LocalLoopbackHelperError{ReasonCode: "cleanup_failed"}
	}
	return r.fakeLocalMountRuntime.ListOwned(ctx)
}

func TestLocalMountRestartUnknownOwnershipNeverReleasesBackend(t *testing.T) {
	for _, failOn := range []int{1, 2} {
		t.Run([]string{"before-remove", "after-remove"}[failOn-1], func(t *testing.T) {
			settings := memory.NewLocalMountRepo()
			if err := settings.SetEnabled(context.Background(), true); err != nil {
				t.Fatal(err)
			}
			runtime := &unknownRestartRuntime{fakeLocalMountRuntime: &fakeLocalMountRuntime{available: true}}
			service := NewLocalMountService(settings, memory.NewTargetRuntimeRepo(), audit.NewMemoryWriter(), TargetRuntimeConfig{})
			service.SetRuntime(newLocalMountServiceResources(t), nil, settings, runtime)
			if _, err := service.Sync(context.Background(), "initial"); err != nil {
				t.Fatal(err)
			}
			runtime.lists = 0
			runtime.failOn = failOn
			if err := service.PrepareRestart(context.Background(), "restart"); err == nil || len(runtime.released) != 0 {
				t.Fatalf("unverified ownership released backend: released=%v err=%v", runtime.released, err)
			}
		})
	}
}

type delayedRestartRuntime struct {
	*fakeLocalMountRuntime
	entered chan struct{}
	release chan struct{}
}

func (r *delayedRestartRuntime) Ensure(ctx context.Context, library domain.LocalLoopbackLibraryMapping, mappings []domain.LocalLoopbackDeviceMapping, descriptors []domain.VTLDeviceDescriptor) ([]LocalLoopbackDeviceObservation, error) {
	close(r.entered)
	<-r.release
	return r.fakeLocalMountRuntime.Ensure(ctx, library, mappings, descriptors)
}

func TestLocalMountRestartStopDeadlineBlocksCleanupUntilWorkerExits(t *testing.T) {
	settings := memory.NewLocalMountRepo()
	runtime := &delayedRestartRuntime{fakeLocalMountRuntime: &fakeLocalMountRuntime{available: true}, entered: make(chan struct{}), release: make(chan struct{})}
	service := NewLocalMountService(settings, memory.NewTargetRuntimeRepo(), audit.NewMemoryWriter(), TargetRuntimeConfig{})
	service.SetRuntime(newLocalMountServiceResources(t), nil, settings, runtime)
	if _, err := service.SetEnabled(context.Background(), true, "initial"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-runtime.entered:
	case <-time.After(time.Second):
		t.Fatal("coordinator never started")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	err := service.Stop(ctx, "shutdown")
	close(runtime.release)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stop did not wait for outstanding coordinator: %v", err)
	}
	waitLocalMountIdle(t, service)
	if len(runtime.removeCalls) != 0 {
		t.Fatal("cleanup ran before coordinator completed")
	}
	if err := service.Stop(context.Background(), "shutdown-retry"); err != nil || len(runtime.owners) != 0 {
		t.Fatalf("stop retry did not remove completed mapping: owners=%v err=%v", runtime.owners, err)
	}
}

func TestLocalMountRestartRetainsDisabledIntent(t *testing.T) {
	settings := memory.NewLocalMountRepo()
	runtime := &fakeLocalMountRuntime{available: true}
	service := NewLocalMountService(settings, memory.NewTargetRuntimeRepo(), audit.NewMemoryWriter(), TargetRuntimeConfig{})
	service.SetRuntime(newLocalMountServiceResources(t), nil, settings, runtime)
	if err := service.PrepareRestart(context.Background(), "restart"); err != nil {
		t.Fatal(err)
	}
	status, err := service.Status(context.Background())
	if err != nil || status.Enabled || status.State != domain.LocalMountStateDisabled {
		t.Fatalf("restart enabled a disabled setting: %+v err=%v", status, err)
	}
}

type unreadableRestartSettings struct{}

func (unreadableRestartSettings) Enabled(context.Context) (bool, error) {
	return false, domain.ErrInvalidState
}
func (unreadableRestartSettings) SetEnabled(context.Context, bool) error { return nil }

func TestLocalMountRestartPrerequisiteFailuresBlockCleanup(t *testing.T) {
	for _, settings := range []LocalMountSettingsRepository{memory.NewLocalMountRepo(), unreadableRestartSettings{}} {
		service := NewLocalMountService(settings, memory.NewTargetRuntimeRepo(), audit.NewMemoryWriter(), TargetRuntimeConfig{})
		if err := service.PrepareRestart(context.Background(), "restart"); err == nil {
			t.Fatal("restart accepted missing runtime or unreadable settings")
		}
	}
}
