package orchestration

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Holo-VTL/Holo/control-plane/internal/audit"
	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
	"github.com/Holo-VTL/Holo/control-plane/internal/repo/memory"
)

func TestLocalMountStatusEmptyDevicesRemainJSONArray(t *testing.T) {
	for _, devices := range [][]domain.LocalMountDeviceStatus{nil, {}} {
		service := NewLocalMountService(memory.NewLocalMountRepo(), memory.NewTargetRuntimeRepo(), audit.NewMemoryWriter(), TargetRuntimeConfig{})
		service.setLast(LocalMountStatus{State: domain.LocalMountStateDisabled, Devices: devices})
		status, err := service.Status(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if status.Devices == nil || len(status.Devices) != 0 {
			t.Fatalf("empty devices must be a non-nil list: %+v", status)
		}
		encoded, err := json.Marshal(status)
		if err != nil {
			t.Fatal(err)
		}
		var response struct {
			Devices json.RawMessage `json:"devices"`
		}
		if err := json.Unmarshal(encoded, &response); err != nil {
			t.Fatal(err)
		}
		if string(response.Devices) != "[]" {
			t.Fatalf("empty device response is not an array: %s", response.Devices)
		}
	}
}

type pendingEnumerationRuntime struct {
	*fakeLocalMountRuntime
	attempts int
}

func (r *pendingEnumerationRuntime) Ensure(ctx context.Context, library domain.LocalLoopbackLibraryMapping, mappings []domain.LocalLoopbackDeviceMapping, descriptors []domain.VTLDeviceDescriptor) ([]LocalLoopbackDeviceObservation, error) {
	observations, err := r.fakeLocalMountRuntime.Ensure(ctx, library, mappings, descriptors)
	r.attempts++
	if r.attempts == 1 {
		for i := range observations {
			observations[i].State = domain.LocalMountDeviceStatePending
			observations[i].ObservedPaths = nil
			observations[i].ReasonCode = domain.LocalMountReasonDeviceNotEnumerated
		}
	}
	return observations, err
}

func TestLocalMountAsyncConfirmsDelayedEnumeration(t *testing.T) {
	settings := memory.NewLocalMountRepo()
	runtime := &pendingEnumerationRuntime{fakeLocalMountRuntime: &fakeLocalMountRuntime{available: true}}
	service := NewLocalMountService(settings, memory.NewTargetRuntimeRepo(), audit.NewMemoryWriter(), TargetRuntimeConfig{})
	service.SetRuntime(newLocalMountServiceResources(t), nil, settings, runtime)
	if _, err := service.SetEnabled(context.Background(), true, "tester"); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(5 * time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		status, err := service.Status(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if status.State == domain.LocalMountStateConnected {
			if status.ConnectedDeviceCount != 3 {
				t.Fatalf("connected without all devices: %+v", status)
			}
			waitLocalMountIdle(t, service)
			return
		}
		select {
		case <-deadline:
			t.Fatalf("pending enumeration was never confirmed: %+v", status)
		case <-ticker.C:
		}
	}
}

func waitLocalMountIdle(t *testing.T, service *LocalMountService) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		service.asyncMu.Lock()
		running := service.asyncRun
		service.asyncMu.Unlock()
		if !running {
			return
		}
		select {
		case <-deadline:
			t.Fatal("local mount worker did not finish")
		case <-ticker.C:
		}
	}
}

func TestLocalMountEnumerationTimeoutIsReportedAndCanBeRetried(t *testing.T) {
	settings := memory.NewLocalMountRepo()
	if err := settings.SetEnabled(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	runtime := &pendingEnumerationRuntime{fakeLocalMountRuntime: &fakeLocalMountRuntime{available: true}}
	service := NewLocalMountService(settings, memory.NewTargetRuntimeRepo(), audit.NewMemoryWriter(), TargetRuntimeConfig{})
	service.SetRuntime(newLocalMountServiceResources(t), nil, settings, runtime)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	service.asyncRun = true
	service.runAsyncSync(ctx, "tester")
	status, err := service.Status(context.Background())
	if err != nil || !status.Enabled || status.State != domain.LocalMountStateFailed || status.LastError != "operation_timeout" {
		t.Fatalf("timeout did not produce a retryable failure: %+v err=%v", status, err)
	}
	for _, device := range status.Devices {
		if device.State != domain.LocalMountDeviceStateFailed || device.ReasonCode != domain.LocalMountReasonOperationTimeout {
			t.Fatalf("device stayed pending after timeout: %+v", device)
		}
	}
	if _, err := service.SetEnabled(context.Background(), true, "retry"); err != nil {
		t.Fatal(err)
	}
	waitLocalMountIdle(t, service)
	status, err = service.Status(context.Background())
	if err != nil || status.State != domain.LocalMountStateConnected || status.LastError != "" {
		t.Fatalf("retry did not recover: %+v err=%v", status, err)
	}
}

type blockedMountRuntime struct {
	*fakeLocalMountRuntime
	entered     chan struct{}
	release     chan struct{}
	nextEntered chan struct{}
	nextRelease chan struct{}
	attempts    int
}

func (r *blockedMountRuntime) Ensure(ctx context.Context, library domain.LocalLoopbackLibraryMapping, mappings []domain.LocalLoopbackDeviceMapping, descriptors []domain.VTLDeviceDescriptor) ([]LocalLoopbackDeviceObservation, error) {
	r.attempts++
	entered, release := r.entered, r.release
	if r.attempts > 1 {
		entered, release = r.nextEntered, r.nextRelease
	}
	if entered == nil {
		return r.fakeLocalMountRuntime.Ensure(ctx, library, mappings, descriptors)
	}
	close(entered)
	select {
	case <-release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return r.fakeLocalMountRuntime.Ensure(ctx, library, mappings, descriptors)
}

func TestLocalMountQueuedEnableDoesNotReusePreviousEnabledCompletion(t *testing.T) {
	ctx := context.Background()
	settings := memory.NewLocalMountRepo()
	runtime := &blockedMountRuntime{fakeLocalMountRuntime: &fakeLocalMountRuntime{available: true}, entered: make(chan struct{}), release: make(chan struct{}), nextEntered: make(chan struct{}), nextRelease: make(chan struct{})}
	service := NewLocalMountService(settings, memory.NewTargetRuntimeRepo(), audit.NewMemoryWriter(), TargetRuntimeConfig{})
	service.SetRuntime(newLocalMountServiceResources(t), nil, settings, runtime)
	if _, err := service.SetEnabled(ctx, true, "first"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-runtime.entered:
	case <-time.After(time.Second):
		t.Fatal("mount never started")
	}
	for _, enabled := range []bool{false, true} {
		if _, err := service.SetEnabled(ctx, enabled, "latest"); err != nil {
			t.Fatal(err)
		}
	}
	close(runtime.release)
	select {
	case <-runtime.nextEntered:
	case <-time.After(time.Second):
		t.Fatal("queued mount never started")
	}
	status, err := service.Status(ctx)
	close(runtime.nextRelease)
	waitLocalMountIdle(t, service)
	if err != nil || !status.Enabled || status.State != domain.LocalMountStateConnecting {
		t.Fatalf("previous enable falsely completed a newer queued enable: %+v err=%v", status, err)
	}
	status, err = service.Status(ctx)
	if err != nil || status.State != domain.LocalMountStateConnected {
		t.Fatalf("latest enable did not complete: %+v err=%v", status, err)
	}
}

func TestLocalMountRapidRequestsFinishWithLatestIntent(t *testing.T) {
	settings := memory.NewLocalMountRepo()
	runtime := &blockedMountRuntime{fakeLocalMountRuntime: &fakeLocalMountRuntime{available: true}, entered: make(chan struct{}), release: make(chan struct{})}
	service := NewLocalMountService(settings, memory.NewTargetRuntimeRepo(), audit.NewMemoryWriter(), TargetRuntimeConfig{})
	service.SetRuntime(newLocalMountServiceResources(t), nil, settings, runtime)
	if _, err := service.SetEnabled(context.Background(), true, "first"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-runtime.entered:
	case <-time.After(time.Second):
		t.Fatal("mount never started")
	}
	for _, enabled := range []bool{false, true, false} {
		if _, err := service.SetEnabled(context.Background(), enabled, "latest"); err != nil {
			t.Fatal(err)
		}
	}
	status, err := service.Status(context.Background())
	if err != nil || status.Enabled || status.State != domain.LocalMountStateDisconnecting {
		t.Fatalf("queued intent was not shown as processing: %+v err=%v", status, err)
	}
	close(runtime.release)
	waitLocalMountIdle(t, service)
	status, err = service.Status(context.Background())
	if err != nil || status.Enabled || status.State != domain.LocalMountStateDisabled || status.ResidualDeviceCount != 0 {
		t.Fatalf("older mount result overwrote latest unmount: %+v err=%v", status, err)
	}
	if len(runtime.ensureCalls) != 1 || len(runtime.removeCalls) != 1 {
		t.Fatalf("requests were not coalesced: ensure=%d remove=%d", len(runtime.ensureCalls), len(runtime.removeCalls))
	}
}

type failingMountSettings struct{ *memory.LocalMountRepo }

func (s failingMountSettings) Enabled(context.Context) (bool, error) {
	return false, errors.New("catalog unavailable")
}

func TestLocalMountEarlySyncFailureClearsProcessingState(t *testing.T) {
	settings := memory.NewLocalMountRepo()
	service := NewLocalMountService(failingMountSettings{settings}, memory.NewTargetRuntimeRepo(), audit.NewMemoryWriter(), TargetRuntimeConfig{})
	service.setLast(LocalMountStatus{Enabled: true, State: domain.LocalMountStateConnecting})
	service.SyncAsync("tester")
	waitLocalMountIdle(t, service)
	status := service.getLast()
	if !status.Enabled || status.State != domain.LocalMountStateFailed || status.LastError != "local mount operation failed" {
		t.Fatalf("early failure left status processing: %+v", status)
	}
}

func TestLocalMountStatusDoesNotReportOldDisabledResultForNewEnabledIntent(t *testing.T) {
	settings := memory.NewLocalMountRepo()
	if err := settings.SetEnabled(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	service := NewLocalMountService(settings, memory.NewTargetRuntimeRepo(), nil, TargetRuntimeConfig{})
	service.setLast(LocalMountStatus{Enabled: false, State: domain.LocalMountStateDisabled, LastSyncAt: timePointer(time.Now())})
	status, err := service.Status(context.Background())
	if err != nil || !status.Enabled || status.State != domain.LocalMountStateConnecting {
		t.Fatalf("old completed result hid current intent: %+v err=%v", status, err)
	}
}

func TestLocalMountPendingDevicesRemainConnectingUntilEnumerationCompletes(t *testing.T) {
	state := resolveLocalMountState(true, 3, 2, 0, []domain.LocalMountDeviceStatus{
		{State: domain.LocalMountDeviceStateConnected},
		{State: domain.LocalMountDeviceStateConnected},
		{State: domain.LocalMountDeviceStatePending},
	}, nil)
	if state != domain.LocalMountStateConnecting {
		t.Fatalf("pending device was reported as a completed partial result: %s", state)
	}
}

func TestLocalMountCompletionStatesDistinguishPendingFailureAndResidue(t *testing.T) {
	for _, tc := range []struct {
		name      string
		enabled   bool
		desired   int
		connected int
		residual  int
		devices   []domain.LocalMountDeviceStatus
		err       error
		want      domain.LocalMountState
	}{
		{name: "empty enabled inventory", enabled: true, want: domain.LocalMountStateConnected},
		{name: "confirmed disabled", want: domain.LocalMountStateDisabled},
		{name: "disabled but residue remains", residual: 1, want: domain.LocalMountStateFailed},
		{name: "disabled but observation failed", err: context.DeadlineExceeded, want: domain.LocalMountStateFailed},
		{name: "all connected", enabled: true, desired: 1, connected: 1, want: domain.LocalMountStateConnected},
		{name: "connected but residue remains", enabled: true, desired: 1, connected: 1, residual: 1, want: domain.LocalMountStatePartial},
		{name: "all failed", enabled: true, desired: 1, devices: []domain.LocalMountDeviceStatus{{State: domain.LocalMountDeviceStateFailed}}, want: domain.LocalMountStateFailed},
		{name: "some failed", enabled: true, desired: 2, connected: 1, devices: []domain.LocalMountDeviceStatus{{State: domain.LocalMountDeviceStateConnected}, {State: domain.LocalMountDeviceStateFailed}}, want: domain.LocalMountStatePartial},
		{name: "removal pending", enabled: true, desired: 1, devices: []domain.LocalMountDeviceStatus{{State: domain.LocalMountDeviceStateRemoving}}, want: domain.LocalMountStateConnecting},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveLocalMountState(tc.enabled, tc.desired, tc.connected, tc.residual, tc.devices, tc.err); got != tc.want {
				t.Fatalf("state=%s want=%s", got, tc.want)
			}
		})
	}
}

func TestLocalMountResidualObservationPreventsFalseCompletion(t *testing.T) {
	status := aggregateLocalMountStatus(true, []domain.VTLDeviceDescriptor{{DeviceKey: "drive:a", Kind: domain.LocalDeviceKindDrive, LibraryID: "lib-a", BackendReady: true}}, map[string]LocalLoopbackDeviceObservation{
		"drive:a": {DeviceKey: "drive:a", State: domain.LocalMountDeviceStateConnected},
	}, []domain.LocalMountDeviceStatus{{DeviceKey: "drive:old", State: domain.LocalMountDeviceStateResidual, ReasonCode: domain.LocalMountReasonCleanupFailed}}, nil)
	if status.State != domain.LocalMountStatePartial || status.ResidualDeviceCount != 1 || status.ConnectedDeviceCount != 1 || status.Devices[1].DisplayName != "drive:a" {
		t.Fatalf("residual observation reported as complete: %+v", status)
	}
	if countResidual([]LocalLoopbackDeviceObservation{{State: domain.LocalMountDeviceStateResidual}, {State: domain.LocalMountDeviceStateConnected}}) != 1 {
		t.Fatal("residual device count includes connected devices")
	}
}

func TestLocalMountTopologyRemovalConfirmsOnlyCurrentDevices(t *testing.T) {
	ctx := context.Background()
	settings := memory.NewLocalMountRepo()
	if err := settings.SetEnabled(ctx, true); err != nil {
		t.Fatal(err)
	}
	resources := newLocalMountServiceResources(t)
	runtime := &fakeLocalMountRuntime{available: true}
	service := NewLocalMountService(settings, memory.NewTargetRuntimeRepo(), audit.NewMemoryWriter(), TargetRuntimeConfig{})
	service.SetRuntime(resources, nil, settings, runtime)
	if _, err := service.Sync(ctx, "initial"); err != nil {
		t.Fatal(err)
	}
	if err := resources.DeleteDrive(ctx, "drive-b"); err != nil {
		t.Fatal(err)
	}
	status, err := service.Sync(ctx, "topology-changed")
	if err != nil || status.State != domain.LocalMountStateConnected || status.DesiredDeviceCount != 2 || status.ConnectedDeviceCount != 2 || status.ResidualDeviceCount != 0 {
		t.Fatalf("topology change did not finish cleanly: %+v err=%v", status, err)
	}
	if len(runtime.released) != 1 || runtime.released[0] != domain.DriveDeviceKey("drive-b") {
		t.Fatalf("wrong backend released: %+v", runtime.released)
	}
	mappings, err := settings.ListDeviceMappings(ctx, "lib-a")
	if err != nil || len(mappings) != 2 {
		t.Fatalf("stale device mapping retained: %+v err=%v", mappings, err)
	}
}

func TestLocalMountBusyRemovalCanBeRetriedWithoutClaimingSuccess(t *testing.T) {
	ctx := context.Background()
	settings := memory.NewLocalMountRepo()
	if err := settings.SetEnabled(ctx, true); err != nil {
		t.Fatal(err)
	}
	runtime := &fakeLocalMountRuntime{available: true}
	service := NewLocalMountService(settings, memory.NewTargetRuntimeRepo(), audit.NewMemoryWriter(), TargetRuntimeConfig{})
	service.SetRuntime(newLocalMountServiceResources(t), nil, settings, runtime)
	if _, err := service.Sync(ctx, "initial"); err != nil {
		t.Fatal(err)
	}
	runtime.removeErr = LocalLoopbackHelperError{ReasonCode: "device_busy"}
	if _, err := service.SetEnabled(ctx, false, "disable"); err != nil {
		t.Fatal(err)
	}
	waitLocalMountIdle(t, service)
	status, err := service.Status(ctx)
	if err != nil || status.Enabled || status.State != domain.LocalMountStateFailed || status.LastError != "device_busy" || len(runtime.released) != 0 {
		t.Fatalf("busy removal was hidden as success: %+v err=%v released=%v", status, err, runtime.released)
	}
	runtime.removeErr = nil
	if _, err := service.SetEnabled(ctx, false, "retry"); err != nil {
		t.Fatal(err)
	}
	waitLocalMountIdle(t, service)
	status, err = service.Status(ctx)
	if err != nil || status.Enabled || status.State != domain.LocalMountStateDisabled || status.LastError != "" || len(runtime.released) != 3 {
		t.Fatalf("removal retry did not finish: %+v err=%v released=%v", status, err, runtime.released)
	}
}

func TestLocalMountMissingRuntimeDoesNotLeaveEnabledIntentProcessing(t *testing.T) {
	ctx := context.Background()
	settings := memory.NewLocalMountRepo()
	service := NewLocalMountService(settings, nil, audit.NewMemoryWriter(), TargetRuntimeConfig{})
	if _, err := service.SetEnabled(ctx, true, "tester"); err != nil {
		t.Fatal(err)
	}
	waitLocalMountIdle(t, service)
	status, err := service.Status(ctx)
	if err != nil || !status.Enabled || status.State != domain.LocalMountStateFailed {
		t.Fatalf("missing runtime left the switch locked: %+v err=%v", status, err)
	}
}
