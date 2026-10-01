package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Holo-VTL/Holo/control-plane/internal/audit"
	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
	"github.com/Holo-VTL/Holo/control-plane/internal/orchestration"
	"github.com/Holo-VTL/Holo/control-plane/internal/repo/memory"
)

type apiISCSISecurityRuntime struct {
	present map[string]bool
}

func (r *apiISCSISecurityRuntime) WithISCSISecurityLock(_ context.Context, operation func() error) error {
	return operation()
}

func (r *apiISCSISecurityRuntime) TargetRuntimeAbsent(_ context.Context, iqn string) (bool, error) {
	return !r.present[iqn], nil
}

func TestISCSISecurityBindingAPIUsesOfflineGuardAndReturnsSources(t *testing.T) {
	ctx := context.Background()
	core := memory.NewCoreResourcesRepo()
	library, _ := domain.NewVirtualLibrary("lib-a", "Library A")
	if err := core.CreateLibrary(ctx, library); err != nil {
		t.Fatal(err)
	}
	securityRepo := memory.NewISCSISecurityRepo()
	credential := domain.ISCSICredential{CredentialID: "cred-a", Label: "backup", Username: "backup-user", EncryptedSecret: make([]byte, 64), Version: 1}
	if err := securityRepo.CreateCredential(ctx, credential); err != nil {
		t.Fatal(err)
	}
	iqn := "iqn.2026-04.ai.holo:drive-a"
	if err := securityRepo.RegisterTarget(ctx, iqn, "lib-a", "drive-a", "drive"); err != nil {
		t.Fatal(err)
	}
	if err := securityRepo.SetTargetOffline(ctx, iqn, true); err != nil {
		t.Fatal(err)
	}
	runtime := &apiISCSISecurityRuntime{present: map[string]bool{}}
	auditWriter := audit.NewMemoryWriter()
	service := orchestration.NewISCSISecurityService(securityRepo, nil, runtime, auditWriter)
	handler := newISCSISecurityHandler(service, core)
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/libraries/{id}/iscsi-security", handler.handleLibraryBinding)
	mux.HandleFunc("/v1/iscsi-security/credentials", handler.handleCredentials)

	protected := "{\"generation\":1,\"auth\":{\"mode\":\"chap\",\"credentialId\":\"cred-a\",\"initiators\":[\"iqn.1991-05.com.microsoft:backup\"]},\"actor\":\"operator\"}"
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodPut, "/v1/libraries/lib-a/iscsi-security", strings.NewReader(protected)))
	if response.Code != http.StatusOK {
		t.Fatalf("expected binding update 200, got %d: %s", response.Code, response.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	binding := body["binding"].(map[string]any)
	if binding["generation"] != float64(1) || binding["auth"].(map[string]any)["mode"] != "chap" {
		t.Fatalf("unexpected binding response: %#v", body)
	}
	events := auditWriter.Events()
	if len(events) != 1 || events[0].Actor != "self-asserted:operator" {
		t.Fatalf("expected audit event to mark the request actor as self-asserted, got %+v", events)
	}
	invalidActor := `{"generation":2,"auth":{"mode":"none"},"actor":"operator\nforged"}`
	response = httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodPut, "/v1/libraries/lib-a/iscsi-security", strings.NewReader(invalidActor)))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("control characters in actor claims should be rejected: status=%d body=%s", response.Code, response.Body.String())
	}
	stored, err := service.GetBinding(ctx, domain.SecurityScopeLibrary, "lib-a")
	if err != nil || stored.Generation != 1 || stored.Authentication.Mode != domain.ISCSIAuthCHAP {
		t.Fatalf("rejected actor claim changed stored policy: %+v, %v", stored, err)
	}
	stale := `{"generation":1,"auth":{"mode":"chap","credentialId":"cred-a","initiators":["iqn.1991-05.com.microsoft:backup"]}}`
	response = httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodPut, "/v1/libraries/lib-a/iscsi-security", strings.NewReader(stale)))
	if response.Code != http.StatusConflict {
		t.Fatalf("stale generation must be rejected: status=%d body=%s", response.Code, response.Body.String())
	}
	runtime.present[iqn] = true
	weakening := "{\"generation\":2,\"auth\":{\"mode\":\"none\"},\"actor\":\"operator\"}"
	response = httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodPut, "/v1/libraries/lib-a/iscsi-security", strings.NewReader(weakening)))
	if response.Code != http.StatusConflict || strings.Contains(response.Body.String(), "target") {
		t.Fatalf("live target weakening should return a generic conflict: status=%d body=%q", response.Code, response.Body.String())
	}
	stored, err = service.GetBinding(ctx, domain.SecurityScopeLibrary, "lib-a")
	if err != nil || stored.Generation != 1 || stored.Authentication.Mode != domain.ISCSIAuthCHAP {
		t.Fatalf("rejected edit changed stored policy: %+v, %v", stored, err)
	}
}

func TestISCSISecurityCredentialListIsWriteOnlyAndRejectsUnknownFields(t *testing.T) {
	ctx := context.Background()
	securityRepo := memory.NewISCSISecurityRepo()
	canary := "secret-canary-never-return"
	credential := domain.ISCSICredential{CredentialID: "cred-a", Label: "backup", Username: "backup-user", EncryptedSecret: []byte(canary + strings.Repeat("x", 40)), Version: 1}
	if err := securityRepo.CreateCredential(ctx, credential); err != nil {
		t.Fatal(err)
	}
	service := orchestration.NewISCSISecurityService(securityRepo, nil, nil, nil)
	handler := newISCSISecurityHandler(service, nil)

	response := httptest.NewRecorder()
	handler.handleCredentials(response, httptest.NewRequest(http.MethodGet, "/v1/iscsi-security/credentials", nil))
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), canary) || strings.Contains(response.Body.String(), "EncryptedSecret") {
		t.Fatalf("credential listing exposed secret material: status=%d body=%s", response.Code, response.Body.String())
	}
	response = httptest.NewRecorder()
	handler.handleCredentials(response, httptest.NewRequest(http.MethodPost, "/v1/iscsi-security/credentials", strings.NewReader("{\"credentialId\":\"cred-b\",\"label\":\"backup\",\"forwardUsername\":\"backup-user\",\"forwardSecret\":\"secret-canary-never-return\",\"unexpected\":\"leak\"}")))
	if response.Code != http.StatusBadRequest || strings.Contains(response.Body.String(), canary) {
		t.Fatalf("unexpected input must be rejected without echo: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestISCSISecurityDriveOverrideAndStableTargetReset(t *testing.T) {
	ctx := context.Background()
	core := memory.NewCoreResourcesRepo()
	library, _ := domain.NewVirtualLibrary("lib-a", "Library A")
	drive, _ := domain.NewVirtualDrive("drive-a", "lib-a", 1)
	if err := core.CreateLibrary(ctx, library); err != nil {
		t.Fatal(err)
	}
	if err := core.CreateDrive(ctx, drive); err != nil {
		t.Fatal(err)
	}
	securityRepo := memory.NewISCSISecurityRepo()
	if err := securityRepo.RegisterTarget(ctx, drive.IQN, "lib-a", "drive-a", "drive"); err != nil {
		t.Fatal(err)
	}
	if err := securityRepo.SetTargetOffline(ctx, drive.IQN, true); err != nil {
		t.Fatal(err)
	}
	service := orchestration.NewISCSISecurityService(securityRepo, nil, &apiISCSISecurityRuntime{present: map[string]bool{}}, nil)
	handler := newISCSISecurityHandler(service, core)
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/drives/{id}/iscsi-security", handler.handleDriveBinding)
	mux.HandleFunc("/v1/iscsi-security/targets/{iqn}", handler.handleTarget)

	driveOverride := "{\"generation\":1,\"auth\":{\"mode\":\"none\",\"initiators\":[\"iqn.1991-05.com.microsoft:backup\"]}}"
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodPut, "/v1/drives/drive-a/iscsi-security", strings.NewReader(driveOverride)))
	if response.Code != http.StatusOK {
		t.Fatalf("drive override failed: status=%d body=%s", response.Code, response.Body.String())
	}
	targetOverride := "{\"generation\":2,\"auth\":null}"
	response = httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodPut, "/v1/iscsi-security/targets/"+drive.IQN, strings.NewReader(targetOverride)))
	if response.Code != http.StatusOK {
		t.Fatalf("target reset failed: status=%d body=%s", response.Code, response.Body.String())
	}
	response = httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/iscsi-security/targets/"+drive.IQN, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("target inventory read failed: status=%d body=%s", response.Code, response.Body.String())
	}
	var targetView struct {
		Resolved struct {
			Auth       domain.ISCSIAuthenticationPolicy
			AuthSource domain.ISCSISecurityOrigin
		}
		Binding domain.ISCSISecurityBinding
	}
	if err := json.Unmarshal(response.Body.Bytes(), &targetView); err != nil {
		t.Fatal(err)
	}
	if targetView.Resolved.AuthSource.Scope != "drive" || len(targetView.Resolved.Auth.Initiators) != 1 || !targetView.Binding.AdministrativeOffline {
		t.Fatalf("target reset did not restore drive inheritance or offline intent: %+v", targetView)
	}
}

func TestISCSISecurityPreviewAndInventoryHandlers(t *testing.T) {
	ctx := context.Background()
	core := memory.NewCoreResourcesRepo()
	library, _ := domain.NewVirtualLibrary("lib-preview", "Preview Library")
	drive, _ := domain.NewVirtualDrive("drive-preview", library.LibraryID, 1)
	if err := core.CreateLibrary(ctx, library); err != nil {
		t.Fatal(err)
	}
	if err := core.CreateDrive(ctx, drive); err != nil {
		t.Fatal(err)
	}
	securityRepo := memory.NewISCSISecurityRepo()
	if err := securityRepo.RegisterTarget(ctx, drive.IQN, library.LibraryID, drive.DriveID, "drive"); err != nil {
		t.Fatal(err)
	}
	if err := securityRepo.SetTargetOffline(ctx, drive.IQN, true); err != nil {
		t.Fatal(err)
	}
	service := orchestration.NewISCSISecurityService(securityRepo, nil, &apiISCSISecurityRuntime{present: map[string]bool{}}, nil)
	handler := newISCSISecurityHandler(service, core)
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/libraries/{id}/iscsi-security", handler.handleLibraryBinding)
	mux.HandleFunc("/v1/libraries/{id}/iscsi-security/preview", func(w http.ResponseWriter, r *http.Request) {
		handler.handleScopedPreview(w, r, domain.SecurityScopeLibrary)
	})
	mux.HandleFunc("/v1/drives/{id}/iscsi-security", handler.handleDriveBinding)
	mux.HandleFunc("/v1/drives/{id}/iscsi-security/preview", func(w http.ResponseWriter, r *http.Request) {
		handler.handleScopedPreview(w, r, domain.SecurityScopeDrive)
	})
	mux.HandleFunc("/v1/iscsi-security/targets", handler.handleTargets)
	mux.HandleFunc("/v1/iscsi-security/targets/{iqn}", handler.handleTarget)
	mux.HandleFunc("/v1/iscsi-security/targets/{iqn}/preview", handler.handleTargetPreview)

	for _, path := range []string{
		"/v1/libraries/lib-preview/iscsi-security",
		"/v1/drives/drive-preview/iscsi-security",
		"/v1/iscsi-security/targets",
	} {
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("inventory request %s failed: status=%d body=%s", path, response.Code, response.Body.String())
		}
	}
	body := `{"generation":1,"auth":{"mode":"none","initiators":["iqn.1991-05.com.microsoft:backup"]}}`
	for _, path := range []string{
		"/v1/libraries/lib-preview/iscsi-security/preview",
		"/v1/drives/drive-preview/iscsi-security/preview",
		"/v1/iscsi-security/targets/" + drive.IQN + "/preview",
	} {
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), drive.IQN) {
			t.Fatalf("preview request %s failed: status=%d body=%s", path, response.Code, response.Body.String())
		}
	}
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/iscsi-security/targets/"+drive.IQN, nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"administrativeOffline":true`) {
		t.Fatalf("target inventory did not expose stable offline intent: status=%d body=%s", response.Code, response.Body.String())
	}
}
