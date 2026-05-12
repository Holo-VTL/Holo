package orchestration

import (
	"context"
	"strings"
	"testing"

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
	outputs  map[string]string
	commands []string
}

func (r *recordingRunner) Run(_ context.Context, command string, args ...string) (string, error) {
	line := command + " " + strings.Join(args, " ")
	r.commands = append(r.commands, line)
	for key, out := range r.outputs {
		if strings.Contains(line, key) {
			return out, nil
		}
	}
	return "", nil
}

func TestLocalMountSyncLogsInDesiredTargetsAndCleansStaleHoloNodes(t *testing.T) {
	ctx := context.Background()
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
		"-m node":    "127.0.0.1:3260,1 iqn.2026-04.cloud.backupnext.holo:stale\n",
		"-m session": "tcp: [1] 127.0.0.1:3260,1 iqn.2026-04.cloud.backupnext.holo:drive-a\n",
	}}
	service := newLocalMountServiceWithRunner(&fakeLocalMountSettings{enabled: true}, targetRepo, audit.NewMemoryWriter(), TargetRuntimeConfig{Mode: "tcmu", PortalHost: "127.0.0.1", PortalPort: 3260}, runner)

	status, err := service.Sync(ctx, "tester")
	if err != nil {
		t.Fatalf("sync failed: %v", err)
	}
	if len(status.DesiredIQNs) != 1 || status.DesiredIQNs[0] != pub.TargetIQN {
		t.Fatalf("unexpected desired iqns: %+v", status.DesiredIQNs)
	}
	joined := strings.Join(runner.commands, "\n")
	for _, want := range []string{
		"-m discovery -t sendtargets -p 127.0.0.1:3260",
		"-m node -T iqn.2026-04.cloud.backupnext.holo:drive-a -p 127.0.0.1:3260 --login",
		"-m node -T iqn.2026-04.cloud.backupnext.holo:stale -p 127.0.0.1:3260 --logout",
		"-m node -o delete -T iqn.2026-04.cloud.backupnext.holo:stale -p 127.0.0.1:3260",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing command %q in:\n%s", want, joined)
		}
	}
}

func TestParseISCSIADMNodes(t *testing.T) {
	nodes := parseISCSIADMNodes("10.0.0.1:3260,1 iqn.2026-04.cloud.backupnext.holo:drive-a\n")
	if len(nodes) != 1 || nodes[0].Portal != "10.0.0.1:3260" || nodes[0].IQN != "iqn.2026-04.cloud.backupnext.holo:drive-a" {
		t.Fatalf("unexpected nodes: %+v", nodes)
	}
}
