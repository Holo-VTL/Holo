package orchestration

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"regexp"
	"strings"

	"github.com/Holo-VTL/Holo/control-plane/internal/audit"
	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
	"github.com/Holo-VTL/Holo/control-plane/internal/storageutil"
)

func (s *TargetRuntimeService) StartValidationRun(ctx context.Context, publicationID, actor string) (*domain.ValidationRun, error) {
	return s.StartValidationRunWithRequest(ctx, publicationID, actor, ValidationRunRequest{})
}

func (s *TargetRuntimeService) StartValidationRunWithRequest(ctx context.Context, publicationID, actor string, req ValidationRunRequest) (*domain.ValidationRun, error) {
	publication, err := s.runtimeRepo.FindPublication(ctx, publicationID)
	if err != nil {
		return nil, err
	}
	if publication.State != domain.PublicationReady {
		return nil, domain.ErrInvalidState
	}
	req = req.Normalize()
	if err := req.Validate(); err != nil {
		return nil, err
	}

	validationID, err := newRuntimeID("val")
	if err != nil {
		return nil, fmt.Errorf("generate validation id: %w", err)
	}
	run, err := domain.NewValidationRun(validationID, publicationID)
	if err != nil {
		return nil, err
	}
	run.Mode = req.Mode

	payload, err := buildValidationPayload(req)
	if err != nil {
		return nil, err
	}

	committedReservation := false
	if s.storageWg != nil && publication.PoolID != "" && len(payload) > 0 {
		if _, warning, reserveErr := s.storageWg.ReserveWrite(ctx, publication.PoolID, int64(len(payload))); reserveErr != nil {
			if reserveErr != domain.ErrNotFound {
				audit.EmitTargetRuntimeEvent(ctx, s.auditW, safeActor(actor), "validate", publicationID, "failure", map[string]any{
					"reason":    "insufficient_storage_capacity",
					"poolId":    publication.PoolID,
					"bytes":     len(payload),
					"mode":      req.Mode,
					"errorHint": reserveErr.Error(),
				})
				return nil, reserveErr
			}
		} else if warning {
			audit.EmitTargetRuntimeEvent(ctx, s.auditW, safeActor(actor), "storage_capacity_warning", publication.PoolID, "success", map[string]any{
				"publicationId": publicationID,
				"bytes":         len(payload),
			})
		}
		committedReservation = true
		defer func() {
			if committedReservation {
				if rollbackErr := s.storageWg.RollbackReservedWrite(ctx, publication.PoolID, int64(len(payload))); rollbackErr != nil {
					log.Printf("storage write reservation rollback failed pool=%s publication=%s err=%v", publication.PoolID, publicationID, rollbackErr)
				}
			}
		}()
	}

	if err := s.runtimeRepo.WriteValidationMedia(ctx, publicationID, payload); err != nil {
		return nil, err
	}
	readback, err := s.runtimeRepo.ReadValidationMedia(ctx, publicationID)
	if err != nil {
		return nil, err
	}

	writeDigest := sha256Hex(payload)
	readDigest := sha256Hex(readback)
	written := int64(len(payload))
	read := int64(len(readback))

	// Defensive guard: if readback was modified, preserve failed evidence.
	if !bytes.Equal(payload, readback) {
		readDigest = "mismatch:" + readDigest
	}

	evidence := fmt.Sprintf("tests/compatibility/iscsi/evidence/%s.json", validationID)
	if err := run.Complete(written, read, writeDigest, readDigest, evidence); err != nil {
		return nil, err
	}
	if err := s.runtimeRepo.SaveValidationRun(ctx, run); err != nil {
		return nil, err
	}
	committedReservation = false

	result := "success"
	if run.Status != domain.ValidationPassed {
		result = "failure"
	}
	audit.EmitTargetRuntimeEvent(ctx, s.auditW, safeActor(actor), "validate", run.ValidationID, result, map[string]any{"publicationId": publicationID, "status": run.Status, "mode": run.Mode, "bytesWritten": run.BytesWritten, "bytesRead": run.BytesRead, "writeDigest": run.WriteDigest, "readDigest": run.ReadDigest})
	return run, nil
}

func validateTargetPublicationForRuntime(publication *domain.TargetPublication) error {
	if publication == nil {
		return domain.ErrInvalidInput
	}
	if _, err := normalizeTargetIQN(publication.TargetIQN, publication.DriveID); err != nil {
		return err
	}
	if strings.TrimSpace(publication.PublicationID) == "" || strings.TrimSpace(publication.PoolID) == "" {
		return domain.ErrInvalidInput
	}
	if !isSafeTargetcliToken(storageutil.SanitizeLayoutID(publication.PoolID)) {
		return domain.ErrInvalidInput
	}
	if !isSafeTargetcliToken(runtimeBackstoreName(publication)) {
		return domain.ErrInvalidInput
	}
	return nil
}

func validateTargetcliArgs(args ...string) error {
	if err := validateTargetcliArgTokens(args...); err != nil {
		return err
	}
	if len(args) < 2 {
		return domain.ErrInvalidInput
	}
	path, action := args[0], args[1]
	rest := args[2:]

	switch {
	case path == "sessions" && action == "detail":
		return requireNoTargetcliArgs(rest)
	case path == "/backstores" && action == "ls":
		return requireNoTargetcliArgs(rest)
	case path == "/backstores/fileio":
		return validateFileioTargetcliCommand(action, rest)
	case strings.HasPrefix(path, "/backstores/user:"):
		return validateUserBackstoreTargetcliCommand(path, action, rest)
	case path == "/iscsi":
		return validateISCSITargetcliCommand(action, rest)
	case strings.HasPrefix(path, "/iscsi/") && strings.HasSuffix(path, "/tpg1"):
		return validateTPGTargetcliCommand(path, action, rest)
	case strings.HasPrefix(path, "/iscsi/") && strings.HasSuffix(path, "/tpg1/luns"):
		return validateLUNTargetcliCommand(path, action, rest)
	default:
		return domain.ErrInvalidInput
	}
}

func validateTargetcliArgTokens(args ...string) error {
	for _, arg := range args {
		if strings.TrimSpace(arg) == "" || strings.ContainsRune(arg, '\x00') || strings.ContainsAny(arg, "\r\n") {
			return domain.ErrInvalidInput
		}
		if strings.Contains(arg, "..") {
			return domain.ErrInvalidInput
		}
	}
	return nil
}

func requireNoTargetcliArgs(args []string) error {
	if len(args) != 0 {
		return domain.ErrInvalidInput
	}
	return nil
}

func validateFileioTargetcliCommand(action string, args []string) error {
	switch action {
	case "create":
		if len(args) != 3 || !hasSafeAssignment(args[0], "name", isSafeTargetcliToken) ||
			!hasAbsolutePathAssignment(args[1], "file_or_dev") || !isSizeArg(args[2]) {
			return domain.ErrInvalidInput
		}
		return nil
	case "delete":
		if len(args) != 1 || !isSafeTargetcliToken(args[0]) {
			return domain.ErrInvalidInput
		}
		return nil
	default:
		return domain.ErrInvalidInput
	}
}

func validateUserBackstoreTargetcliCommand(path, action string, args []string) error {
	subtype := strings.TrimPrefix(path, "/backstores/user:")
	if !targetcliSubtypePattern.MatchString(subtype) {
		return domain.ErrInvalidInput
	}
	switch action {
	case "create":
		if len(args) != 3 || !hasSafeAssignment(args[0], "name", isSafeTargetcliToken) ||
			!isSizeArg(args[1]) || !hasAbsolutePathAssignment(args[2], "cfgstring") {
			return domain.ErrInvalidInput
		}
		return nil
	case "delete":
		if len(args) != 1 || !isSafeTargetcliToken(args[0]) {
			return domain.ErrInvalidInput
		}
		return nil
	default:
		return domain.ErrInvalidInput
	}
}

func validateISCSITargetcliCommand(action string, args []string) error {
	if len(args) != 1 || (action != "create" && action != "delete") {
		return domain.ErrInvalidInput
	}
	_, err := normalizeTargetIQN(args[0], "")
	return err
}

func validateTPGTargetcliCommand(path, action string, args []string) error {
	iqn, ok := iqnFromTPGPath(path, "/tpg1")
	if !ok {
		return domain.ErrInvalidInput
	}
	if _, err := normalizeTargetIQN(iqn, ""); err != nil {
		return err
	}
	if action != "set" || len(args) < 2 {
		return domain.ErrInvalidInput
	}
	switch args[0] {
	case "attribute":
		for _, item := range args[1:] {
			if !containsString(tcmuTargetcliTPGAttributes, item) {
				return domain.ErrInvalidInput
			}
		}
		return nil
	case "parameter":
		if len(args) != 2 {
			return domain.ErrInvalidInput
		}
		if containsString(tcmuISCSIDataPathParameters, args[1]) {
			return nil
		}
		return domain.ErrInvalidInput
	default:
		return domain.ErrInvalidInput
	}
}

func validateLUNTargetcliCommand(path, action string, args []string) error {
	iqn, ok := iqnFromTPGPath(path, "/tpg1/luns")
	if !ok {
		return domain.ErrInvalidInput
	}
	if _, err := normalizeTargetIQN(iqn, ""); err != nil {
		return err
	}
	if action != "create" || len(args) != 1 {
		return domain.ErrInvalidInput
	}
	return validateBackstoreRef(args[0])
}

func iqnFromTPGPath(path, suffix string) (string, bool) {
	if !strings.HasPrefix(path, "/iscsi/") || !strings.HasSuffix(path, suffix) {
		return "", false
	}
	iqn := strings.TrimSuffix(strings.TrimPrefix(path, "/iscsi/"), suffix)
	iqn = strings.Trim(iqn, "/")
	return iqn, iqn != ""
}

func validateBackstoreRef(ref string) error {
	if strings.HasPrefix(ref, "/backstores/fileio/") {
		name := strings.TrimPrefix(ref, "/backstores/fileio/")
		if isSafeTargetcliToken(name) {
			return nil
		}
		return domain.ErrInvalidInput
	}
	if strings.HasPrefix(ref, "/backstores/user:") {
		parts := strings.Split(strings.TrimPrefix(ref, "/backstores/user:"), "/")
		if len(parts) == 2 && targetcliSubtypePattern.MatchString(parts[0]) && isSafeTargetcliToken(parts[1]) {
			return nil
		}
	}
	return domain.ErrInvalidInput
}

func hasSafeAssignment(arg, key string, validator func(string) bool) bool {
	value, ok := strings.CutPrefix(arg, key+"=")
	return ok && validator(value)
}

func hasAbsolutePathAssignment(arg, key string) bool {
	value, ok := strings.CutPrefix(arg, key+"=")
	return ok && strings.HasPrefix(value, "/") && !strings.Contains(value, "..") && !strings.ContainsAny(value, "\x00\r\n")
}

func isSizeArg(arg string) bool {
	value, ok := strings.CutPrefix(arg, "size=")
	if !ok || !strings.HasSuffix(value, "M") {
		return false
	}
	digits := strings.TrimSuffix(value, "M")
	if digits == "" {
		return false
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func isSafeTargetcliToken(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && targetcliTokenPattern.MatchString(value)
}

var targetIQNPattern = regexp.MustCompile(`^iqn\.\d{4}-\d{2}\.[a-z0-9][a-z0-9.-]*:[a-z0-9][a-z0-9:.-]*$`)
var iqnValuePattern = regexp.MustCompile(`^iqn\.\d{4}-\d{2}\.[a-z0-9][a-z0-9.-]*:[a-z0-9][a-z0-9:._-]*$`)
var iqnInTextPattern = regexp.MustCompile(`iqn\.\d{4}-\d{2}\.[a-z0-9][a-z0-9.-]*:[a-z0-9][a-z0-9:._-]*`)

func normalizeTargetIQN(targetIQN, driveID string) (string, error) {
	iqn := strings.TrimSpace(strings.ToLower(targetIQN))
	if iqn == "" {
		token := sanitizeIQNToken(driveID)
		iqn = fmt.Sprintf("iqn.2026-04.cloud.backupnext.holo:%s", token)
	}

	if domain.ValidateTargetIQN(iqn) != nil || !targetIQNPattern.MatchString(iqn) {
		return "", domain.ErrInvalidInput
	}
	return iqn, nil
}

func sanitizeIQNToken(raw string) string {
	raw = strings.ToLower(strings.TrimSpace(raw))
	var b strings.Builder
	for _, r := range raw {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	token := strings.Trim(b.String(), "-.")
	if token == "" {
		token = "drive"
	}
	return token
}

func newRuntimeID(prefix string) (string, error) {
	var entropy [16]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return "", err
	}
	return prefix + "-" + hex.EncodeToString(entropy[:]), nil
}

const (
	defaultValidationBytes int64 = 4096
	maxValidationBytes     int64 = 1 << 20
)

func buildValidationPayload(req ValidationRunRequest) ([]byte, error) {
	switch req.Mode {
	case domain.ValidationModeFixed:
		size := req.Bytes
		if size == 0 {
			size = defaultValidationBytes
		}
		if size < 0 || size > maxValidationBytes {
			return nil, domain.ErrInvalidInput
		}
		pattern := []byte(req.Pattern)
		if len(pattern) == 0 {
			pattern = []byte("HOLO")
		}
		payload := make([]byte, int(size))
		for i := range payload {
			payload[i] = pattern[i%len(pattern)]
		}
		return payload, nil
	case domain.ValidationModeEmpty:
		if req.Bytes > 0 {
			return nil, domain.ErrInvalidInput
		}
		return []byte{}, nil
	default:
		return nil, domain.ErrInvalidInput
	}
}

func sha256Hex(payload []byte) string {
	sum := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(sum[:])
}
