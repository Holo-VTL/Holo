package orchestration

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Holo-VTL/Holo/control-plane/internal/audit"
	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
)

type LocalMountSettingsRepository interface {
	Enabled(ctx context.Context) (bool, error)
	SetEnabled(ctx context.Context, enabled bool) error
}

type LocalMountStatus struct {
	Enabled     bool       `json:"enabled"`
	DesiredIQNs []string   `json:"desiredIqns"`
	MountedIQNs []string   `json:"mountedIqns"`
	LastSyncAt  *time.Time `json:"lastSyncAt,omitempty"`
	LastError   string     `json:"lastError,omitempty"`
}

type iscsiNode struct {
	Portal string
	IQN    string
}

type LocalMountService struct {
	settings   LocalMountSettingsRepository
	targets    TargetRuntimeRepository
	runner     commandRunner
	auditW     audit.Writer
	cfg        TargetRuntimeConfig
	syncMu     sync.Mutex
	lastMu     sync.RWMutex
	last       LocalMountStatus
	asyncMu    sync.Mutex
	asyncRun   bool
	asyncNext  bool
	asyncActor string
}

func NewLocalMountService(settings LocalMountSettingsRepository, targets TargetRuntimeRepository, auditW audit.Writer, cfg TargetRuntimeConfig) *LocalMountService {
	return newLocalMountServiceWithRunner(settings, targets, auditW, cfg, &osCommandRunner{})
}

func newLocalMountServiceWithRunner(settings LocalMountSettingsRepository, targets TargetRuntimeRepository, auditW audit.Writer, cfg TargetRuntimeConfig, runner commandRunner) *LocalMountService {
	if runner == nil {
		runner = &osCommandRunner{}
	}
	return &LocalMountService{
		settings: settings,
		targets:  targets,
		runner:   runner,
		auditW:   auditW,
		cfg:      normalizeTargetRuntimeConfig(cfg),
	}
}

func (s *LocalMountService) Status(ctx context.Context) (LocalMountStatus, error) {
	enabled, err := s.settings.Enabled(ctx)
	if err != nil {
		return LocalMountStatus{}, err
	}
	status := s.getLast()
	status.Enabled = enabled
	status.DesiredIQNs = nodeIQNs(desiredNodes(s.targets.ListPublications(ctx), s.cfg))
	return status, nil
}

func (s *LocalMountService) SetEnabled(ctx context.Context, enabled bool, actor string) (LocalMountStatus, error) {
	if err := s.settings.SetEnabled(ctx, enabled); err != nil {
		return LocalMountStatus{}, err
	}
	status, err := s.Sync(ctx, actor)
	if err != nil {
		return status, err
	}
	return status, nil
}

func (s *LocalMountService) Sync(ctx context.Context, actor string) (LocalMountStatus, error) {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()

	enabled, err := s.settings.Enabled(ctx)
	if err != nil {
		return LocalMountStatus{}, err
	}
	publications := s.targets.ListPublications(ctx)
	desired := desiredNodes(publications, s.cfg)
	now := time.Now().UTC()
	status := LocalMountStatus{
		Enabled:     enabled,
		DesiredIQNs: nodeIQNs(desired),
		LastSyncAt:  &now,
	}
	if strings.EqualFold(s.cfg.Mode, "in-memory") {
		s.setLast(status)
		return status, nil
	}
	previous := s.getLast()

	var syncErr error
	if enabled {
		for _, node := range desired {
			if err := s.loginNode(ctx, node); err != nil && syncErr == nil {
				syncErr = err
			}
		}
	}
	existing, listErr := s.listConfiguredNodes(ctx)
	if listErr != nil && syncErr == nil {
		syncErr = listErr
	}
	desiredSet := make(map[string]bool, len(desired))
	for _, node := range desired {
		desiredSet[node.IQN] = enabled && samePortal(node.Portal, localMountPortal(s.cfg))
	}
	for _, node := range existing {
		if samePortal(node.Portal, localMountPortal(s.cfg)) && isHoloIQN(node.IQN) && !desiredSet[node.IQN] {
			if err := s.deleteNode(ctx, node); err != nil && syncErr == nil {
				syncErr = err
			}
		}
	}
	mounted, sessionErr := s.listMountedIQNs(ctx)
	if sessionErr != nil && syncErr == nil {
		syncErr = sessionErr
	}
	status.MountedIQNs = mounted
	if syncErr != nil {
		status.LastError = syncErr.Error()
		if localMountStatusChanged(previous, status) {
			audit.EmitTargetRuntimeEvent(ctx, s.auditW, safeActor(actor), "local_mount_sync", "local", "failure", map[string]any{"error": syncErr.Error(), "enabled": enabled})
		}
		s.setLast(status)
		return status, syncErr
	}
	if localMountStatusChanged(previous, status) {
		audit.EmitTargetRuntimeEvent(ctx, s.auditW, safeActor(actor), "local_mount_sync", "local", "success", map[string]any{"enabled": enabled, "desired": len(status.DesiredIQNs), "mounted": len(status.MountedIQNs)})
	}
	s.setLast(status)
	return status, nil
}

func (s *LocalMountService) SyncAsync(actor string) {
	if s == nil {
		return
	}
	s.asyncMu.Lock()
	if s.asyncRun {
		s.asyncNext = true
		s.asyncActor = actor
		s.asyncMu.Unlock()
		return
	}
	s.asyncRun = true
	s.asyncActor = actor
	s.asyncMu.Unlock()
	go s.runAsyncSync(actor)
}

func (s *LocalMountService) runAsyncSync(actor string) {
	for {
		_, _ = s.Sync(context.Background(), actor)

		s.asyncMu.Lock()
		if !s.asyncNext {
			s.asyncRun = false
			s.asyncMu.Unlock()
			return
		}
		actor = s.asyncActor
		s.asyncNext = false
		s.asyncMu.Unlock()
	}
}

func (s *LocalMountService) getLast() LocalMountStatus {
	s.lastMu.RLock()
	defer s.lastMu.RUnlock()
	return s.last
}

func (s *LocalMountService) setLast(status LocalMountStatus) {
	s.lastMu.Lock()
	defer s.lastMu.Unlock()
	s.last = status
}

func (s *LocalMountService) loginNode(ctx context.Context, node iscsiNode) error {
	if node.IQN == "" || node.Portal == "" {
		return domain.ErrInvalidInput
	}
	if _, err := s.runISCSI(ctx, "discover", node.Portal); err != nil {
		return fmt.Errorf("discover local target %s: %w", node.IQN, err)
	}
	if _, err := s.runISCSI(ctx, "set-startup", node.IQN, node.Portal); err != nil {
		return fmt.Errorf("enable automatic local login %s: %w", node.IQN, err)
	}
	if _, err := s.runISCSI(ctx, "login", node.IQN, node.Portal); err != nil && !isIgnorableISCSIADMError(err) {
		return fmt.Errorf("login local target %s: %w", node.IQN, err)
	}
	return nil
}

func (s *LocalMountService) deleteNode(ctx context.Context, node iscsiNode) error {
	if _, err := s.runISCSI(ctx, "logout", node.IQN, node.Portal); err != nil && !isIgnorableISCSIADMError(err) {
		return fmt.Errorf("logout stale local target %s: %w", node.IQN, err)
	}
	if _, err := s.runISCSI(ctx, "delete", node.IQN, node.Portal); err != nil && !isIgnorableISCSIADMError(err) {
		return fmt.Errorf("delete stale local target %s: %w", node.IQN, err)
	}
	return nil
}

func (s *LocalMountService) runISCSI(ctx context.Context, args ...string) (string, error) {
	cmdCtx, cancel := context.WithTimeout(ctx, localMountCommandTimeout())
	defer cancel()
	if s.cfg.UseSudo {
		helper := strings.TrimSpace(os.Getenv("HOLO_ISCSI_PRIVILEGED_HELPER"))
		if helper == "" {
			helper = "holo-iscsi-helper"
		}
		return s.runner.Run(cmdCtx, "sudo", append([]string{"-n", helper}, args...)...)
	}
	if helper := strings.TrimSpace(os.Getenv("HOLO_ISCSI_PRIVILEGED_HELPER")); helper != "" {
		return s.runner.Run(cmdCtx, helper, args...)
	}
	iscsiArgs, err := iscsiadmArgs(args...)
	if err != nil {
		return "", err
	}
	return s.runner.Run(cmdCtx, "iscsiadm", iscsiArgs...)
}

func (s *LocalMountService) listConfiguredNodes(ctx context.Context) ([]iscsiNode, error) {
	out, err := s.runISCSI(ctx, "nodes")
	if err != nil && !isIgnorableISCSIADMError(err) {
		return nil, err
	}
	return parseISCSIADMNodes(out), nil
}

func (s *LocalMountService) listMountedIQNs(ctx context.Context) ([]string, error) {
	out, err := s.runISCSI(ctx, "sessions")
	if err != nil && !isIgnorableISCSIADMError(err) {
		return nil, err
	}
	iqns := make([]string, 0)
	for _, node := range parseISCSIADMNodes(out) {
		if samePortal(node.Portal, localMountPortal(s.cfg)) && isHoloIQN(node.IQN) {
			iqns = append(iqns, node.IQN)
		}
	}
	sort.Strings(iqns)
	return iqns, nil
}

func desiredNodes(publications []*domain.TargetPublication, cfg TargetRuntimeConfig) []iscsiNode {
	nodes := make([]iscsiNode, 0)
	seen := make(map[string]bool)
	for _, publication := range publications {
		if publication == nil || publication.State != domain.PublicationReady {
			continue
		}
		iqn := strings.TrimSpace(publication.TargetIQN)
		if iqn == "" || seen[iqn] {
			continue
		}
		portal := strings.TrimSpace(publication.Portal)
		if portal == "" {
			portal = fmt.Sprintf("%s:%d", cfg.PortalHost, cfg.PortalPort)
		}
		nodes = append(nodes, iscsiNode{Portal: portal, IQN: iqn})
		seen[iqn] = true
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].IQN < nodes[j].IQN })
	return nodes
}

func nodeIQNs(nodes []iscsiNode) []string {
	iqns := make([]string, 0, len(nodes))
	for _, node := range nodes {
		iqns = append(iqns, node.IQN)
	}
	sort.Strings(iqns)
	return iqns
}

func localMountStatusChanged(previous, current LocalMountStatus) bool {
	return previous.Enabled != current.Enabled ||
		previous.LastError != current.LastError ||
		!sameStringSet(previous.DesiredIQNs, current.DesiredIQNs) ||
		!sameStringSet(previous.MountedIQNs, current.MountedIQNs)
}

func sameStringSet(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	leftCopy := append([]string(nil), left...)
	rightCopy := append([]string(nil), right...)
	sort.Strings(leftCopy)
	sort.Strings(rightCopy)
	for i := range leftCopy {
		if leftCopy[i] != rightCopy[i] {
			return false
		}
	}
	return true
}

func parseISCSIADMNodes(raw string) []iscsiNode {
	var nodes []iscsiNode
	for _, line := range strings.Split(raw, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 2 {
			continue
		}
		iqn := fields[len(fields)-1]
		portal := fields[len(fields)-2]
		portal = strings.TrimSuffix(strings.Split(portal, ",")[0], "]")
		portal = strings.TrimPrefix(portal, "[")
		if strings.HasPrefix(iqn, "iqn.") && portal != "" {
			nodes = append(nodes, iscsiNode{Portal: portal, IQN: iqn})
		}
	}
	return nodes
}

func isHoloIQN(iqn string) bool {
	iqn = strings.ToLower(strings.TrimSpace(iqn))
	return strings.HasPrefix(iqn, "iqn.") && strings.Contains(iqn, ".holo:")
}

func isIgnorableISCSIADMError(err error) bool {
	if err == nil {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "already present") ||
		strings.Contains(msg, "already exists") ||
		strings.Contains(msg, "already logged in") ||
		strings.Contains(msg, "no records found") ||
		strings.Contains(msg, "no active sessions") ||
		strings.Contains(msg, "not found")
}

func localMountPortal(cfg TargetRuntimeConfig) string {
	return fmt.Sprintf("%s:%d", strings.TrimSpace(cfg.PortalHost), cfg.PortalPort)
}

func samePortal(left, right string) bool {
	return strings.EqualFold(strings.TrimSpace(left), strings.TrimSpace(right))
}

func localMountCommandTimeout() time.Duration {
	raw := strings.TrimSpace(os.Getenv("HOLO_LOCAL_MOUNT_ISCSIADM_TIMEOUT_SEC"))
	if raw == "" {
		return 8 * time.Second
	}
	seconds, err := strconv.Atoi(raw)
	if err != nil || seconds <= 0 || seconds > 120 {
		return 8 * time.Second
	}
	return time.Duration(seconds) * time.Second
}

func iscsiadmArgs(args ...string) ([]string, error) {
	if len(args) == 0 {
		return nil, domain.ErrInvalidInput
	}
	switch args[0] {
	case "discover":
		if len(args) != 2 {
			return nil, domain.ErrInvalidInput
		}
		return []string{"-m", "discovery", "-t", "sendtargets", "-p", args[1]}, nil
	case "set-startup":
		if len(args) != 3 {
			return nil, domain.ErrInvalidInput
		}
		return []string{"-m", "node", "-T", args[1], "-p", args[2], "--op", "update", "-n", "node.startup", "-v", "automatic"}, nil
	case "login":
		if len(args) != 3 {
			return nil, domain.ErrInvalidInput
		}
		return []string{"-m", "node", "-T", args[1], "-p", args[2], "--login"}, nil
	case "logout":
		if len(args) != 3 {
			return nil, domain.ErrInvalidInput
		}
		return []string{"-m", "node", "-T", args[1], "-p", args[2], "--logout"}, nil
	case "delete":
		if len(args) != 3 {
			return nil, domain.ErrInvalidInput
		}
		return []string{"-m", "node", "-o", "delete", "-T", args[1], "-p", args[2]}, nil
	case "nodes":
		if len(args) != 1 {
			return nil, domain.ErrInvalidInput
		}
		return []string{"-m", "node"}, nil
	case "sessions":
		if len(args) != 1 {
			return nil, domain.ErrInvalidInput
		}
		return []string{"-m", "session"}, nil
	default:
		return nil, domain.ErrInvalidInput
	}
}
