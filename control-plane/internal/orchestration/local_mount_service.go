package orchestration

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Holo-VTL/Holo/control-plane/internal/audit"
	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
)

type LocalMountSettingsRepository interface {
	Enabled(ctx context.Context) (bool, error)
	SetEnabled(ctx context.Context, enabled bool) error
}

type LocalMountStatus struct {
	Enabled     bool      `json:"enabled"`
	DesiredIQNs []string  `json:"desiredIqns"`
	MountedIQNs []string  `json:"mountedIqns"`
	LastSyncAt  time.Time `json:"lastSyncAt,omitempty"`
	LastError   string    `json:"lastError,omitempty"`
}

type iscsiNode struct {
	Portal string
	IQN    string
}

type LocalMountService struct {
	settings LocalMountSettingsRepository
	targets  TargetRuntimeRepository
	runner   commandRunner
	auditW   audit.Writer
	cfg      TargetRuntimeConfig
	last     LocalMountStatus
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
	status := s.last
	status.Enabled = enabled
	status.DesiredIQNs = desiredIQNs(s.targets.ListPublications(ctx))
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
	enabled, err := s.settings.Enabled(ctx)
	if err != nil {
		return LocalMountStatus{}, err
	}
	publications := s.targets.ListPublications(ctx)
	desired := desiredNodes(publications, s.cfg)
	status := LocalMountStatus{
		Enabled:     enabled,
		DesiredIQNs: nodeIQNs(desired),
		LastSyncAt:  time.Now().UTC(),
	}
	if strings.EqualFold(s.cfg.Mode, "in-memory") {
		s.last = status
		return status, nil
	}

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
		desiredSet[node.IQN] = enabled
	}
	for _, node := range existing {
		if isHoloIQN(node.IQN) && !desiredSet[node.IQN] {
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
		audit.EmitTargetRuntimeEvent(ctx, s.auditW, safeActor(actor), "local_mount_sync", "local", "failure", map[string]any{"error": syncErr.Error(), "enabled": enabled})
		s.last = status
		return status, syncErr
	}
	audit.EmitTargetRuntimeEvent(ctx, s.auditW, safeActor(actor), "local_mount_sync", "local", "success", map[string]any{"enabled": enabled, "desired": len(status.DesiredIQNs), "mounted": len(status.MountedIQNs)})
	s.last = status
	return status, nil
}

func (s *LocalMountService) loginNode(ctx context.Context, node iscsiNode) error {
	if node.IQN == "" || node.Portal == "" {
		return domain.ErrInvalidInput
	}
	if _, err := s.runISCSIADM(ctx, "-m", "discovery", "-t", "sendtargets", "-p", node.Portal); err != nil {
		return fmt.Errorf("discover local target %s: %w", node.IQN, err)
	}
	if _, err := s.runISCSIADM(ctx, "-m", "node", "-T", node.IQN, "-p", node.Portal, "--op", "update", "-n", "node.startup", "-v", "automatic"); err != nil {
		return fmt.Errorf("enable automatic local login %s: %w", node.IQN, err)
	}
	if _, err := s.runISCSIADM(ctx, "-m", "node", "-T", node.IQN, "-p", node.Portal, "--login"); err != nil && !isIgnorableISCSIADMError(err) {
		return fmt.Errorf("login local target %s: %w", node.IQN, err)
	}
	return nil
}

func (s *LocalMountService) deleteNode(ctx context.Context, node iscsiNode) error {
	if _, err := s.runISCSIADM(ctx, "-m", "node", "-T", node.IQN, "-p", node.Portal, "--logout"); err != nil && !isIgnorableISCSIADMError(err) {
		return fmt.Errorf("logout stale local target %s: %w", node.IQN, err)
	}
	if _, err := s.runISCSIADM(ctx, "-m", "node", "-o", "delete", "-T", node.IQN, "-p", node.Portal); err != nil && !isIgnorableISCSIADMError(err) {
		return fmt.Errorf("delete stale local target %s: %w", node.IQN, err)
	}
	return nil
}

func (s *LocalMountService) runISCSIADM(ctx context.Context, args ...string) (string, error) {
	if s.cfg.UseSudo {
		return s.runner.Run(ctx, "sudo", append([]string{"-n", "iscsiadm"}, args...)...)
	}
	return s.runner.Run(ctx, "iscsiadm", args...)
}

func (s *LocalMountService) listConfiguredNodes(ctx context.Context) ([]iscsiNode, error) {
	out, err := s.runISCSIADM(ctx, "-m", "node")
	if err != nil && !isIgnorableISCSIADMError(err) {
		return nil, err
	}
	return parseISCSIADMNodes(out), nil
}

func (s *LocalMountService) listMountedIQNs(ctx context.Context) ([]string, error) {
	out, err := s.runISCSIADM(ctx, "-m", "session")
	if err != nil && !isIgnorableISCSIADMError(err) {
		return nil, err
	}
	iqns := make([]string, 0)
	for _, node := range parseISCSIADMNodes(out) {
		if isHoloIQN(node.IQN) {
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

func desiredIQNs(publications []*domain.TargetPublication) []string {
	return nodeIQNs(desiredNodes(publications, DefaultTargetRuntimeConfig()))
}

func nodeIQNs(nodes []iscsiNode) []string {
	iqns := make([]string, 0, len(nodes))
	for _, node := range nodes {
		iqns = append(iqns, node.IQN)
	}
	sort.Strings(iqns)
	return iqns
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
	return strings.HasPrefix(iqn, "iqn.2026-04.") && strings.Contains(iqn, ".holo:")
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
