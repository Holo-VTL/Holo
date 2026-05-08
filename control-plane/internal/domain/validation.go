package domain

import (
	"regexp"
	"strings"
	"unicode"
)

const (
	maxManagementIDLen      = 128
	maxManagementLabelLen   = 256
	maxManagementProfileLen = 128
	maxTargetIQNLen         = 223
)

var (
	managementIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._: -]*$`)
	targetIQNPattern    = regexp.MustCompile(`^iqn\.[0-9]{4}-[0-9]{2}\.[a-z0-9][a-z0-9.-]*:[a-z0-9][a-z0-9:._-]*$`)
	devicePathPattern   = regexp.MustCompile(`^/dev/[A-Za-z0-9._/-]+$`)
	profilePattern      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._: /-]*$`)
)

func ValidateManagementID(value string) error {
	if hasControlRune(value) {
		return ErrInvalidInput
	}
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maxManagementIDLen {
		return ErrInvalidInput
	}
	if strings.ContainsAny(value, `/\`) || strings.Contains(value, "..") {
		return ErrInvalidInput
	}
	if !managementIDPattern.MatchString(value) {
		return ErrInvalidInput
	}
	return nil
}

func ValidateManagementLabel(value string, required bool) error {
	if hasControlRune(value) {
		return ErrInvalidInput
	}
	value = strings.TrimSpace(value)
	if value == "" {
		if required {
			return ErrInvalidInput
		}
		return nil
	}
	if len(value) > maxManagementLabelLen {
		return ErrInvalidInput
	}
	return nil
}

func ValidateProfileToken(value string) error {
	if hasControlRune(value) {
		return ErrInvalidInput
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	if len(value) > maxManagementProfileLen ||
		strings.Contains(value, "..") ||
		strings.Contains(value, `\`) ||
		strings.HasPrefix(value, "/") ||
		strings.HasSuffix(value, "/") {
		return ErrInvalidInput
	}
	if !profilePattern.MatchString(value) {
		return ErrInvalidInput
	}
	return nil
}

func ValidateTargetIQN(value string) error {
	if hasControlRune(value) {
		return ErrInvalidInput
	}
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" || len(value) > maxTargetIQNLen {
		return ErrInvalidInput
	}
	if strings.ContainsAny(value, `/\ `) || strings.Contains(value, "..") {
		return ErrInvalidInput
	}
	if !targetIQNPattern.MatchString(value) {
		return ErrInvalidInput
	}
	return nil
}

func ValidateDevicePath(value string) error {
	if hasControlRune(value) {
		return ErrInvalidInput
	}
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 255 {
		return ErrInvalidInput
	}
	if strings.Contains(value, "..") || !strings.HasPrefix(value, "/dev/") {
		return ErrInvalidInput
	}
	if !devicePathPattern.MatchString(value) {
		return ErrInvalidInput
	}
	return nil
}

func ValidatePermission(value PolicyPermission) error {
	if value != PermissionAllow && value != PermissionDeny {
		return ErrInvalidInput
	}
	return nil
}

func hasControlRune(value string) bool {
	for _, r := range value {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}
