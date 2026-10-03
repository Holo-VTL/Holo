package orchestration

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
)

func (s *TargetRuntimeService) ListPublicationsWithConnectedHosts(ctx context.Context) []*domain.TargetPublication {
	publications := s.runtimeRepo.ListPublications(ctx)
	if !hasReadyPublications(publications) {
		return publications
	}

	byTarget, err := s.connectedHostSummaries(ctx)
	if err != nil {
		log.Printf("target session discovery unavailable err=%v", err)
		for _, publication := range publications {
			if publication == nil || publication.State != domain.PublicationReady {
				continue
			}
			publication.ConnectedHosts = &domain.ConnectedHosts{
				Available:    false,
				HostCount:    0,
				SessionCount: 0,
				Initiators:   []string{},
				LastError:    "session discovery unavailable",
			}
		}
		return publications
	}

	for _, publication := range publications {
		if publication == nil || publication.State != domain.PublicationReady {
			continue
		}
		summary := byTarget[strings.ToLower(strings.TrimSpace(publication.TargetIQN))]
		if summary == nil {
			summary = &domain.ConnectedHosts{Available: true, Initiators: []string{}}
		}
		publication.ConnectedHosts = cloneConnectedHosts(summary)
	}
	return publications
}

func hasReadyPublications(publications []*domain.TargetPublication) bool {
	for _, publication := range publications {
		if publication != nil && publication.State == domain.PublicationReady {
			return true
		}
	}
	return false
}

func (s *TargetRuntimeService) connectedHostSummaries(ctx context.Context) (map[string]*domain.ConnectedHosts, error) {
	now := time.Now()
	s.sessionMu.Lock()
	if now.Before(s.sessionCache.expiresAt) {
		result := s.sessionCache.result
		s.sessionMu.Unlock()
		return cloneConnectedHostMap(result.hosts), result.err
	}
	if s.sessionInflight != nil {
		inflight := s.sessionInflight
		s.sessionMu.Unlock()
		select {
		case <-inflight.done:
			return cloneConnectedHostMap(inflight.result.hosts), inflight.result.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	inflight := &targetSessionInflight{done: make(chan struct{})}
	s.sessionInflight = inflight
	s.sessionMu.Unlock()

	result := s.discoverConnectedHosts(ctx)

	s.sessionMu.Lock()
	inflight.result = result
	s.sessionCache = targetSessionCache{
		expiresAt: time.Now().Add(connectedHostsCacheTTL),
		result:    result,
	}
	s.sessionInflight = nil
	close(inflight.done)
	s.sessionMu.Unlock()

	return cloneConnectedHostMap(result.hosts), result.err
}

func (s *TargetRuntimeService) discoverConnectedHosts(ctx context.Context) (result targetSessionResult) {
	defer func() {
		if recovered := recover(); recovered != nil {
			result = targetSessionResult{err: fmt.Errorf("target session discovery panic: %v", recovered)}
		}
	}()

	sessions, err := s.adapter.ListSessions(ctx)
	result.err = err
	if err == nil {
		result.hosts = connectedHostsByTarget(sessions)
	}
	return result
}

func cloneConnectedHostMap(in map[string]*domain.ConnectedHosts) map[string]*domain.ConnectedHosts {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]*domain.ConnectedHosts, len(in))
	for key, summary := range in {
		out[key] = cloneConnectedHosts(summary)
	}
	return out
}

func cloneConnectedHosts(in *domain.ConnectedHosts) *domain.ConnectedHosts {
	if in == nil {
		return nil
	}
	out := *in
	out.Initiators = append([]string(nil), in.Initiators...)
	return &out
}

func connectedHostsByTarget(sessions []TargetSession) map[string]*domain.ConnectedHosts {
	type accumulator struct {
		initiators map[string]struct{}
		sessions   int
	}
	acc := make(map[string]*accumulator)
	for _, session := range sessions {
		targetIQN := strings.ToLower(strings.TrimSpace(session.TargetIQN))
		initiatorIQN := strings.ToLower(strings.TrimSpace(session.InitiatorIQN))
		if !targetIQNPattern.MatchString(targetIQN) || !iqnValuePattern.MatchString(initiatorIQN) {
			continue
		}
		item := acc[targetIQN]
		if item == nil {
			item = &accumulator{initiators: make(map[string]struct{})}
			acc[targetIQN] = item
		}
		item.sessions++
		item.initiators[initiatorIQN] = struct{}{}
	}

	out := make(map[string]*domain.ConnectedHosts, len(acc))
	for targetIQN, item := range acc {
		initiators := make([]string, 0, len(item.initiators))
		for initiator := range item.initiators {
			initiators = append(initiators, initiator)
		}
		sort.Strings(initiators)
		out[targetIQN] = &domain.ConnectedHosts{
			Available:    true,
			HostCount:    len(initiators),
			SessionCount: item.sessions,
			Initiators:   initiators,
		}
	}
	return out
}

func parseTargetcliSessions(output string) []TargetSession {
	var sessions []TargetSession
	currentTarget := ""
	for _, rawLine := range strings.Split(output, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		lower := strings.ToLower(line)
		iqns := iqnInTextPattern.FindAllString(lower, -1)
		if len(iqns) == 0 {
			continue
		}
		if isTargetcliTargetLine(lower) {
			currentTarget = iqns[0]
			continue
		}
		if isTargetcliInitiatorLine(lower) {
			initiator := iqns[len(iqns)-1]
			target := currentTarget
			if len(iqns) > 1 && strings.Contains(lower, "/acls/iqn.") {
				target = iqns[0]
			} else if target == "" && len(iqns) > 1 {
				target = iqns[0]
			}
			if target != "" && target != initiator {
				sessions = append(sessions, TargetSession{
					TargetIQN:    target,
					InitiatorIQN: initiator,
				})
				if len(sessions) >= maxTargetSessionRows {
					log.Printf("target session discovery truncated at %d rows", maxTargetSessionRows)
					return sessions
				}
			}
		}
	}
	return sessions
}

func listConfigfsTargetSessions(root string) ([]TargetSession, error) {
	pattern := filepath.Join(root, "iqn.*", "tpgt_1", "dynamic_sessions")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, err
	}
	if len(matches) == 0 {
		return nil, nil
	}

	sessions := make([]TargetSession, 0)
	for _, path := range matches {
		targetIQN := strings.ToLower(filepath.Base(filepath.Dir(filepath.Dir(path))))
		if !targetIQNPattern.MatchString(targetIQN) {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read dynamic sessions for %s: %w", targetIQN, err)
		}
		for _, initiator := range parseConfigfsDynamicSessions(raw) {
			sessions = append(sessions, TargetSession{
				TargetIQN:    targetIQN,
				InitiatorIQN: initiator,
			})
			if len(sessions) >= maxTargetSessionRows {
				log.Printf("configfs target session discovery truncated at %d rows", maxTargetSessionRows)
				return sessions, nil
			}
		}
	}
	return sessions, nil
}

func parseConfigfsDynamicSessions(raw []byte) []string {
	text := strings.ReplaceAll(string(raw), "\x00", "\n")
	var initiators []string
	seen := make(map[string]struct{})
	for _, field := range strings.Fields(text) {
		initiator := strings.ToLower(strings.TrimSpace(field))
		if !iqnValuePattern.MatchString(initiator) {
			continue
		}
		if _, exists := seen[initiator]; exists {
			continue
		}
		seen[initiator] = struct{}{}
		initiators = append(initiators, initiator)
	}
	sort.Strings(initiators)
	return initiators
}

func fallbackIscsiadmSessions(ctx context.Context, runner commandRunner, useSudo bool, current []TargetSession) []TargetSession {
	out, err := runIscsiadmSessionsOutput(ctx, runner, useSudo)
	if err != nil {
		if !isNoActiveIscsiadmSessions(err) && !isIscsiadmUnavailable(err) {
			log.Printf("iscsi session fallback unavailable err=%v", err)
		}
		return current
	}
	sessions := parseIscsiadmSessions(out)
	if len(sessions) == 0 {
		return current
	}
	return sessions
}

func runIscsiadmSessionsOutput(ctx context.Context, runner commandRunner, useSudo bool) (string, error) {
	timeout := targetcliTimeout()
	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd, args := iscsiadmCommand(useSudo, "-m", "session", "-P", "3")
	out, err := runner.Run(timeoutCtx, cmd, args...)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(timeoutCtx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("iscsiadm timed out after %s", timeout)
		}
		return "", err
	}
	return out, nil
}

func parseIscsiadmSessions(output string) []TargetSession {
	var sessions []TargetSession
	current := TargetSession{}
	flush := func() {
		if current.TargetIQN != "" && current.InitiatorIQN != "" && len(sessions) < maxTargetSessionRows {
			sessions = append(sessions, current)
		}
		current = TargetSession{}
	}

	for _, rawLine := range strings.Split(output, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		lower := strings.ToLower(line)
		switch {
		case strings.HasPrefix(lower, "target:"):
			flush()
			iqns := iqnInTextPattern.FindAllString(lower, -1)
			if len(iqns) > 0 {
				current.TargetIQN = iqns[0]
			}
		case strings.HasPrefix(lower, "iface initiatorname:"):
			iqns := iqnInTextPattern.FindAllString(lower, -1)
			if len(iqns) > 0 {
				current.InitiatorIQN = iqns[0]
			}
		case strings.HasPrefix(lower, "iface ipaddress:"):
			current.SourceAddress = strings.TrimSpace(strings.TrimPrefix(line, "Iface IPaddress:"))
		case strings.HasPrefix(lower, "sid:"):
			current.SessionID = strings.TrimSpace(strings.TrimPrefix(line, "SID:"))
		}
		if len(sessions) >= maxTargetSessionRows {
			log.Printf("iscsi session discovery truncated at %d rows", maxTargetSessionRows)
			return sessions
		}
	}
	flush()
	if len(sessions) > maxTargetSessionRows {
		log.Printf("iscsi session discovery truncated at %d rows", maxTargetSessionRows)
		return sessions[:maxTargetSessionRows]
	}
	return sessions
}

func isNoActiveIscsiadmSessions(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "no active sessions")
}

func isIscsiadmUnavailable(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "executable file not found") ||
		strings.Contains(msg, "command not found") ||
		strings.Contains(msg, "no such file or directory")
}

func isTargetcliTargetLine(lower string) bool {
	return strings.HasPrefix(lower, "target:") ||
		strings.HasPrefix(lower, "o- iqn.") ||
		strings.HasPrefix(lower, "+- iqn.") ||
		(strings.Contains(lower, "/iscsi/iqn.") && !strings.Contains(lower, "/acls/iqn."))
}

func isTargetcliInitiatorLine(lower string) bool {
	return strings.HasPrefix(lower, "initiator:") ||
		strings.HasPrefix(lower, "acl:") ||
		strings.Contains(lower, "/acls/iqn.") ||
		strings.Contains(lower, "mapped lun")
}
