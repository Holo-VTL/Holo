package orchestration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"strings"
)

const maxISCSISecurityHelperInput = 1 << 20

var ErrISCSISecurityHelperUnavailable = errors.New("iSCSI security helper unavailable")

type ISCSISecurityHelperResponse struct {
	OK       bool
	Code     string
	Revision string
	Ready    *bool
}

type iscsiSecurityHelperRunner interface {
	Run(context.Context, string, []byte) ([]byte, error)
}

type ISCSISecurityHelper struct {
	binaryPath string
	runner     iscsiSecurityHelperRunner
}

func NewISCSISecurityHelper(binaryPath string, useSudo bool) *ISCSISecurityHelper {
	return &ISCSISecurityHelper{binaryPath: binaryPath, runner: execISCSISecurityHelperRunner{useSudo: useSudo}}
}

func (h *ISCSISecurityHelper) Call(ctx context.Context, request map[string]any) (ISCSISecurityHelperResponse, error) {
	if h == nil || h.runner == nil || !strings.HasPrefix(h.binaryPath, "/") || strings.Contains(h.binaryPath, "..") || request == nil {
		return ISCSISecurityHelperResponse{}, ErrISCSISecurityHelperUnavailable
	}
	operation, ok := request["operation"].(string)
	if !ok || !allowedISCSISecurityHelperOperation(operation) {
		return ISCSISecurityHelperResponse{}, ErrISCSISecurityHelperUnavailable
	}
	if version, ok := request["version"].(int); !ok || version != 1 {
		return ISCSISecurityHelperResponse{}, ErrISCSISecurityHelperUnavailable
	}
	input, err := json.Marshal(request)
	if err != nil || len(input) == 0 || len(input) > maxISCSISecurityHelperInput {
		return ISCSISecurityHelperResponse{}, ErrISCSISecurityHelperUnavailable
	}
	output, err := h.runner.Run(ctx, h.binaryPath, input)
	if err != nil || len(output) == 0 || len(output) > maxISCSISecurityHelperInput {
		return ISCSISecurityHelperResponse{}, ErrISCSISecurityHelperUnavailable
	}
	var response ISCSISecurityHelperResponse
	decoder := json.NewDecoder(bytes.NewReader(output))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&response); err != nil || !knownISCSISecurityHelperCode(response.Code) {
		return ISCSISecurityHelperResponse{}, ErrISCSISecurityHelperUnavailable
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return ISCSISecurityHelperResponse{}, ErrISCSISecurityHelperUnavailable
	}
	if !response.OK {
		return response, ErrISCSISecurityHelperUnavailable
	}
	return response, nil
}

func (h *ISCSISecurityHelper) ProvisionEmptyVault(ctx context.Context) error {
	_, err := h.Call(ctx, map[string]any{"version": 1, "operation": "vault-provision"})
	return err
}

func (h *ISCSISecurityHelper) StartupCheck(ctx context.Context) error {
	response, err := h.Call(ctx, map[string]any{"version": 1, "operation": "startup-check"})
	if err != nil || response.Ready == nil || !*response.Ready {
		return ErrISCSISecurityHelperUnavailable
	}
	return nil
}

func allowedISCSISecurityHelperOperation(operation string) bool {
	switch operation {
	case "capabilities", "vault-provision", "inspect-target", "create-target-protected", "delete-owned-target",
		"startup-check":
		return true
	default:
		return false
	}
}

func knownISCSISecurityHelperCode(code string) bool {
	switch code {
	case "ok", "invalid_request", "request_too_large", "not_authorized", "unsupported_runtime",
		"target_missing", "target_busy", "target_ready", "capability_missing", "operation_failed":
		return true
	default:
		return false
	}
}

type execISCSISecurityHelperRunner struct {
	useSudo bool
}

func (r execISCSISecurityHelperRunner) Run(ctx context.Context, binaryPath string, input []byte) ([]byte, error) {
	command := binaryPath
	args := []string(nil)
	if r.useSudo {
		command = "sudo"
		args = []string{"-n", "--", binaryPath}
	}
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Stdin = bytes.NewReader(input)
	var output limitedISCSIHelperOutput
	cmd.Stdout = &output
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil || output.overflow {
		return nil, ErrISCSISecurityHelperUnavailable
	}
	return output.data, nil
}

type limitedISCSIHelperOutput struct {
	data     []byte
	overflow bool
}

func (b *limitedISCSIHelperOutput) Write(p []byte) (int, error) {
	if len(b.data)+len(p) > maxISCSISecurityHelperInput {
		b.overflow = true
		return 0, ErrISCSISecurityHelperUnavailable
	}
	b.data = append(b.data, p...)
	return len(p), nil
}
