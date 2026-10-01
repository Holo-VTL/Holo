package orchestration

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type fakeISCSISecurityHelperRunner struct {
	path  string
	input []byte
	reply []byte
	err   error
}

func (r *fakeISCSISecurityHelperRunner) Run(_ context.Context, path string, input []byte) ([]byte, error) {
	r.path = path
	r.input = append([]byte(nil), input...)
	return r.reply, r.err
}

func TestISCSISecurityHelperUsesFixedPathAndBoundedStdinEnvelope(t *testing.T) {
	runner := &fakeISCSISecurityHelperRunner{reply: []byte("{\"ok\":true,\"code\":\"ok\",\"revision\":\"v1\"}")}
	helper := &ISCSISecurityHelper{binaryPath: "/opt/holo/bin/holo-iscsi-security-helper", runner: runner}
	request := map[string]any{"version": 1, "operation": "create-target-protected", "secret": "canary-secret-123"}
	response, err := helper.Call(context.Background(), request)
	if err != nil || !response.OK {
		t.Fatalf("expected helper success, got %+v, %v", response, err)
	}
	if runner.path != helper.binaryPath {
		t.Fatalf("helper path changed: %q", runner.path)
	}
	var got map[string]any
	if err := json.Unmarshal(runner.input, &got); err != nil || got["secret"] != "canary-secret-123" {
		t.Fatalf("secret must travel only in JSON stdin: %s, %v", runner.input, err)
	}
}

func TestISCSISecurityHelperStartupCheckRequiresReadyResult(t *testing.T) {
	for _, test := range []struct {
		name  string
		reply string
		want  bool
	}{
		{name: "ready", reply: `{"ok":true,"code":"ok","ready":true}`},
		{name: "not ready", reply: `{"ok":true,"code":"ok","ready":false}`, want: true},
		{name: "missing readiness", reply: `{"ok":true,"code":"ok"}`, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &fakeISCSISecurityHelperRunner{reply: []byte(test.reply)}
			helper := &ISCSISecurityHelper{binaryPath: "/opt/holo/bin/holo-iscsi-security-helper", runner: runner}
			err := helper.StartupCheck(context.Background())
			if (err != nil) != test.want {
				t.Fatalf("StartupCheck() err=%v, want error=%t", err, test.want)
			}
			var request map[string]any
			if decodeErr := json.Unmarshal(runner.input, &request); decodeErr != nil || request["operation"] != "startup-check" {
				t.Fatalf("startup preflight used unexpected request %s: %v", runner.input, decodeErr)
			}
		})
	}
}

func TestISCSISecurityHelperRejectsUnsupportedAndOversizedRequests(t *testing.T) {
	runner := &fakeISCSISecurityHelperRunner{reply: []byte("{\"ok\":true,\"code\":\"ok\"}")}
	helper := &ISCSISecurityHelper{binaryPath: "/opt/holo/bin/holo-iscsi-security-helper", runner: runner}
	if _, err := helper.Call(context.Background(), map[string]any{"version": 1, "operation": "exec"}); !errors.Is(err, ErrISCSISecurityHelperUnavailable) {
		t.Fatalf("arbitrary operation should be rejected: %v", err)
	}
	if runner.input != nil {
		t.Fatal("unsupported operation must not reach the runner")
	}
	if _, err := helper.Call(context.Background(), map[string]any{"version": 1, "operation": "capabilities", "padding": string(make([]byte, maxISCSISecurityHelperInput))}); !errors.Is(err, ErrISCSISecurityHelperUnavailable) {
		t.Fatalf("oversized request should be rejected: %v", err)
	}
}

func TestISCSISecurityHelperRejectsUnsafeOutputAndDoesNotExposeRunnerError(t *testing.T) {
	runner := &fakeISCSISecurityHelperRunner{reply: []byte("{\"ok\":false,\"code\":\"invalid_request\",\"detail\":\"private-secret\"}")}
	helper := &ISCSISecurityHelper{binaryPath: "/opt/holo/bin/holo-iscsi-security-helper", runner: runner}
	if _, err := helper.Call(context.Background(), map[string]any{"version": 1, "operation": "capabilities"}); !errors.Is(err, ErrISCSISecurityHelperUnavailable) {
		t.Fatalf("unknown output fields should be rejected: %v", err)
	}
	runner.reply = nil
	runner.err = errors.New("private-secret in stderr")
	if _, err := helper.Call(context.Background(), map[string]any{"version": 1, "operation": "capabilities"}); err != ErrISCSISecurityHelperUnavailable {
		t.Fatalf("raw helper error must not escape: %v", err)
	}
}

func TestExecISCSISecurityHelperRunnerUsesNoArgumentsAndDiscardsStderr(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "helper")
	script := "#!/bin/sh\n[ \"$#\" -eq 0 ] || exit 22\ncat >/dev/null\nprintf '{\"ok\":true,\"code\":\"ok\"}'\nprintf 'secret-canary-in-stderr' >&2\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	output, err := (execISCSISecurityHelperRunner{}).Run(context.Background(), binary, []byte(`{"operation":"create-target-protected","secret":"secret-canary"}`))
	if err != nil || string(output) != `{"ok":true,"code":"ok"}` {
		t.Fatalf("fixed helper execution exposed argv/stderr or failed: output=%q err=%v", output, err)
	}
}
