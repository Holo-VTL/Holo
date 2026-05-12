package orchestration

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Holo-VTL/Holo/control-plane/internal/audit"
	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
	"github.com/Holo-VTL/Holo/control-plane/internal/repo/memory"
)

type fakeLocalMountSettings struct {
	enabled bool
}

func (s *fakeLocalMountSettings) Enabled(context.Context) (bool, error) {
	return s.enabled, nil
}

func (s *fakeLocalMountSettings) SetEnabled(_ context.Context, enabled bool) error {
	s.enabled = enabled
	return nil
}

type recordingRunner struct {
	mu       sync.Mutex
	outputs  map[string]string
	commands []string
}

func (r *recordingRunner) Run(_ context.Context, command string, args ...string) (string, error) {
	line := command + " " + strings.Join(args, " ")
	r.mu.Lock()
	defer r.mu.Unlock()
	r.commands = append(r.commands, line)
	if out, ok := r.outputs[line]; ok {
		return out, nil
	}
	return "", nil
}

func (r *recordingRunner) snapshotCommands() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.commands...)
}

func TestLocalMountSyncLogsInDesiredTargetsAndCleansStaleHoloNodes(t *testing.T) {
	ctx := context.Background()
	t.Setenv("HOLO_ISCSI_PRIVILEGED_HELPER", "/opt/holo/bin/holo-iscsi-helper")
	targetRepo := memory.NewTargetRuntimeRepo()
	pub, err := domain.NewTargetPublication("pub-a", "pool-a", "lib-a", "drive-a", "cart-a", "iqn.2026-04.cloud.backupnext.holo:drive-a")
	if err != nil {
		t.Fatalf("new publication: %v", err)
	}
	if err := pub.MarkReady("127.0.0.1:3260"); err != nil {
		t.Fatalf("mark ready: %v", err)
	}
	if err := targetRepo.SavePublication(ctx, pub); err != nil {
		t.Fatalf("save publication: %v", err)
	}
	runner := &recordingRunner{outputs: map[string]string{
		"sudo -n /opt/holo/bin/holo-iscsi-helper nodes":    "127.0.0.1:3260,1 iqn.2026-04.cloud.backupnext.holo:stale\n10.0.0.2:3260,1 iqn.2026-04.cloud.backupnext.holo:remote\n",
		"sudo -n /opt/holo/bin/holo-iscsi-helper sessions": "tcp: [1] 127.0.0.1:3260,1 iqn.2026-04.cloud.backupnext.holo:drive-a\ntcp: [2] 10.0.0.2:3260,1 iqn.2026-04.cloud.backupnext.holo:remote\n",
	}}
	service := newLocalMountServiceWithRunner(&fakeLocalMountSettings{enabled: true}, targetRepo, audit.NewMemoryWriter(), TargetRuntimeConfig{Mode: "tcmu", PortalHost: "127.0.0.1", PortalPort: 3260, UseSudo: true}, runner)

	status, err := service.Sync(ctx, "tester")
	if err != nil {
		t.Fatalf("sync failed: %v", err)
	}
	if len(status.DesiredIQNs) != 1 || status.DesiredIQNs[0] != pub.TargetIQN {
		t.Fatalf("unexpected desired iqns: %+v", status.DesiredIQNs)
	}
	for _, want := range []string{
		"sudo -n /opt/holo/bin/holo-iscsi-helper discover 127.0.0.1:3260",
		"sudo -n /opt/holo/bin/holo-iscsi-helper login iqn.2026-04.cloud.backupnext.holo:drive-a 127.0.0.1:3260",
		"sudo -n /opt/holo/bin/holo-iscsi-helper logout iqn.2026-04.cloud.backupnext.holo:stale 127.0.0.1:3260",
		"sudo -n /opt/holo/bin/holo-iscsi-helper delete iqn.2026-04.cloud.backupnext.holo:stale 127.0.0.1:3260",
	} {
		commands := runner.snapshotCommands()
		if !hasLocalMountCommand(commands, want) {
			t.Fatalf("missing command %q in:\n%s", want, strings.Join(commands, "\n"))
		}
	}
	for _, unwanted := range []string{
		"sudo -n /opt/holo/bin/holo-iscsi-helper logout iqn.2026-04.cloud.backupnext.holo:remote 10.0.0.2:3260",
		"sudo -n /opt/holo/bin/holo-iscsi-helper delete iqn.2026-04.cloud.backupnext.holo:remote 10.0.0.2:3260",
	} {
		commands := runner.snapshotCommands()
		if hasLocalMountCommand(commands, unwanted) {
			t.Fatalf("unexpected cross-portal cleanup command %q in:\n%s", unwanted, strings.Join(commands, "\n"))
		}
	}
	if len(status.MountedIQNs) != 1 || status.MountedIQNs[0] != pub.TargetIQN {
		t.Fatalf("unexpected mounted iqns: %+v", status.MountedIQNs)
	}
}

func TestParseISCSIADMNodes(t *testing.T) {
	nodes := parseISCSIADMNodes("10.0.0.1:3260,1 iqn.2026-04.cloud.backupnext.holo:drive-a\n")
	if len(nodes) != 1 || nodes[0].Portal != "10.0.0.1:3260" || nodes[0].IQN != "iqn.2026-04.cloud.backupnext.holo:drive-a" {
		t.Fatalf("unexpected nodes: %+v", nodes)
	}
}

func TestIsHoloIQNAllowsFutureDates(t *testing.T) {
	if !isHoloIQN("iqn.2027-01.cloud.backupnext.holo:drive-a") {
		t.Fatal("expected future Holo IQN to be managed")
	}
}

func TestLocalMountSyncAsyncCoalescesPendingRequests(t *testing.T) {
	runner := &blockingLocalMountRunner{
		outputs: map[string]string{
			"iscsiadm -m node":    "",
			"iscsiadm -m session": "",
		},
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	service := newLocalMountServiceWithRunner(&fakeLocalMountSettings{enabled: false}, memory.NewTargetRuntimeRepo(), audit.NewMemoryWriter(), TargetRuntimeConfig{Mode: "tcmu", PortalHost: "127.0.0.1", PortalPort: 3260}, runner)

	service.SyncAsync("tester")
	select {
	case <-runner.entered:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for first async sync")
	}
	for i := 0; i < 5; i++ {
		service.SyncAsync("tester")
	}
	close(runner.release)
	waitForAsyncLocalMount(t, service)

	if got := runner.countCommand("iscsiadm -m node"); got != 2 {
		t.Fatalf("expected one running sync plus one coalesced sync, got %d node scans", got)
	}
}

func TestLocalMountAuditOnlyEmitsOnStatusChange(t *testing.T) {
	ctx := context.Background()
	writer := audit.NewMemoryWriter()
	runner := &recordingRunner{outputs: map[string]string{
		"iscsiadm -m node":    "",
		"iscsiadm -m session": "",
	}}
	service := newLocalMountServiceWithRunner(&fakeLocalMountSettings{enabled: false}, memory.NewTargetRuntimeRepo(), writer, TargetRuntimeConfig{Mode: "tcmu", PortalHost: "127.0.0.1", PortalPort: 3260}, runner)

	if _, err := service.Sync(ctx, "tester"); err != nil {
		t.Fatalf("first sync failed: %v", err)
	}
	if _, err := service.Sync(ctx, "tester"); err != nil {
		t.Fatalf("second sync failed: %v", err)
	}
	if got := len(writer.Events()); got != 0 {
		t.Fatalf("expected no repeated no-op sync audit events, got %d", got)
	}
}

type blockingLocalMountRunner struct {
	mu       sync.Mutex
	outputs  map[string]string
	commands []string
	entered  chan struct{}
	release  chan struct{}
	once     sync.Once
}

func (r *blockingLocalMountRunner) Run(_ context.Context, command string, args ...string) (string, error) {
	line := command + " " + strings.Join(args, " ")
	r.mu.Lock()
	r.commands = append(r.commands, line)
	r.mu.Unlock()
	if line == "iscsiadm -m node" {
		r.once.Do(func() {
			close(r.entered)
			<-r.release
		})
	}
	if out, ok := r.outputs[line]; ok {
		return out, nil
	}
	return "", nil
}

func (r *blockingLocalMountRunner) countCommand(want string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	count := 0
	for _, command := range r.commands {
		if command == want {
			count++
		}
	}
	return count
}

func waitForAsyncLocalMount(t *testing.T, service *LocalMountService) {
	t.Helper()
	deadline := time.After(time.Second)
	for {
		service.asyncMu.Lock()
		running := service.asyncRun
		service.asyncMu.Unlock()
		if !running {
			return
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for async sync to drain")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func hasLocalMountCommand(commands []string, want string) bool {
	for _, command := range commands {
		if command == want {
			return true
		}
	}
	return false
}
