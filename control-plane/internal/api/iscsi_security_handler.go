package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
	"github.com/Holo-VTL/Holo/control-plane/internal/orchestration"
)

type iscsiSecurityHandler struct {
	service   *orchestration.ISCSISecurityService
	resources interface {
		FindLibrary(context.Context, string) (*domain.VirtualLibrary, error)
		FindDrive(context.Context, string) (*domain.VirtualDrive, error)
	}
}

type iscsiBindingPutRequest struct {
	Generation int64
	Auth       json.RawMessage
	Actor      string
}

func newISCSISecurityHandler(service *orchestration.ISCSISecurityService, resources interface {
	FindLibrary(context.Context, string) (*domain.VirtualLibrary, error)
	FindDrive(context.Context, string) (*domain.VirtualDrive, error)
}) *iscsiSecurityHandler {
	return &iscsiSecurityHandler{service: service, resources: resources}
}

func (h *iscsiSecurityHandler) handleLibraryBinding(w http.ResponseWriter, r *http.Request) {
	h.handleScopedBinding(w, r, domain.SecurityScopeLibrary, r.PathValue("id"))
}

func (h *iscsiSecurityHandler) handleDriveBinding(w http.ResponseWriter, r *http.Request) {
	h.handleScopedBinding(w, r, domain.SecurityScopeDrive, r.PathValue("id"))
}

func (h *iscsiSecurityHandler) handleScopedBinding(w http.ResponseWriter, r *http.Request, scope domain.SecurityScope, ownerID string) {
	ownerID = strings.TrimSpace(ownerID)
	if domain.ValidateManagementID(ownerID) != nil {
		respondSecurityError(w, domain.ErrInvalidInput)
		return
	}
	var libraryID string
	switch scope {
	case domain.SecurityScopeLibrary:
		library, err := h.resources.FindLibrary(r.Context(), ownerID)
		if err != nil {
			respondSecurityError(w, err)
			return
		}
		libraryID = library.LibraryID
	case domain.SecurityScopeDrive:
		drive, err := h.resources.FindDrive(r.Context(), ownerID)
		if err != nil {
			respondSecurityError(w, err)
			return
		}
		libraryID = drive.LibraryID
	default:
		respondSecurityError(w, domain.ErrInvalidInput)
		return
	}

	switch r.Method {
	case http.MethodGet:
		binding, err := h.service.GetBinding(r.Context(), scope, ownerID)
		if err != nil {
			respondSecurityError(w, err)
			return
		}
		views, err := h.service.ListTargets(r.Context())
		if err != nil {
			respondSecurityError(w, err)
			return
		}
		filtered := make([]orchestration.ISCSISecurityTargetView, 0)
		for _, view := range views {
			if scope == domain.SecurityScopeLibrary && view.Binding.LibraryID == libraryID ||
				scope == domain.SecurityScopeDrive && view.Binding.DriveID == ownerID && view.Binding.DeviceRole == "drive" {
				filtered = append(filtered, view)
			}
		}
		respondJSON(w, http.StatusOK, map[string]any{"binding": binding, "targets": filtered})
	case http.MethodPut:
		req, authPolicy, err := decodeISCSIBindingPut(r)
		if err != nil {
			respondSecurityError(w, err)
			return
		}
		binding := domain.ISCSISecurityBinding{Scope: scope, OwnerID: ownerID, LibraryID: libraryID, Generation: req.Generation, Authentication: authPolicy}
		if scope == domain.SecurityScopeLibrary {
			binding.LibraryID = ""
		}
		if err := h.service.PutBinding(r.Context(), binding, req.Actor); err != nil {
			respondSecurityError(w, err)
			return
		}
		respondJSON(w, http.StatusOK, map[string]any{"binding": binding})
	default:
		respondError(w, http.StatusMethodNotAllowed, "method not allowed", nil)
	}
}

func (h *iscsiSecurityHandler) handleScopedPreview(w http.ResponseWriter, r *http.Request, scope domain.SecurityScope) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "method not allowed", nil)
		return
	}
	ownerID := strings.TrimSpace(r.PathValue("id"))
	req, authPolicy, err := decodeISCSIBindingPut(r)
	if err != nil {
		respondSecurityError(w, err)
		return
	}
	var binding domain.ISCSISecurityBinding
	switch scope {
	case domain.SecurityScopeLibrary:
		if domain.ValidateManagementID(ownerID) != nil {
			respondSecurityError(w, domain.ErrInvalidInput)
			return
		}
		if _, err := h.resources.FindLibrary(r.Context(), ownerID); err != nil {
			respondSecurityError(w, err)
			return
		}
		binding = domain.ISCSISecurityBinding{Scope: scope, OwnerID: ownerID, Generation: req.Generation, Authentication: authPolicy}
	case domain.SecurityScopeDrive:
		if domain.ValidateManagementID(ownerID) != nil {
			respondSecurityError(w, domain.ErrInvalidInput)
			return
		}
		drive, err := h.resources.FindDrive(r.Context(), ownerID)
		if err != nil {
			respondSecurityError(w, err)
			return
		}
		binding = domain.ISCSISecurityBinding{Scope: scope, OwnerID: ownerID, LibraryID: drive.LibraryID, Generation: req.Generation, Authentication: authPolicy}
	default:
		respondSecurityError(w, domain.ErrInvalidInput)
		return
	}
	impact, err := h.service.PreviewBinding(r.Context(), binding)
	if err != nil {
		respondSecurityError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"targets": impact})
}

func (h *iscsiSecurityHandler) handleTargets(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "method not allowed", nil)
		return
	}
	targets, err := h.service.ListTargets(r.Context())
	if err != nil {
		respondSecurityError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"targets": targets})
}

func (h *iscsiSecurityHandler) handleTarget(w http.ResponseWriter, r *http.Request) {
	targetIQN := strings.TrimSpace(r.PathValue("iqn"))
	switch r.Method {
	case http.MethodGet:
		view, err := h.service.ResolveTarget(r.Context(), targetIQN)
		if err != nil {
			respondSecurityError(w, err)
			return
		}
		respondJSON(w, http.StatusOK, view)
	case http.MethodPut:
		req, authPolicy, err := decodeISCSIBindingPut(r)
		if err != nil {
			respondSecurityError(w, err)
			return
		}
		existing, err := h.service.ResolveTarget(r.Context(), targetIQN)
		if err != nil {
			respondSecurityError(w, err)
			return
		}
		binding := existing.Binding
		binding.Generation = req.Generation
		binding.Authentication = authPolicy
		if err := h.service.PutBinding(r.Context(), binding, req.Actor); err != nil {
			respondSecurityError(w, err)
			return
		}
		respondJSON(w, http.StatusOK, map[string]any{"binding": binding})
	default:
		respondError(w, http.StatusMethodNotAllowed, "method not allowed", nil)
	}
}

func (h *iscsiSecurityHandler) handleTargetPreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "method not allowed", nil)
		return
	}
	targetIQN := strings.TrimSpace(r.PathValue("iqn"))
	req, authPolicy, err := decodeISCSIBindingPut(r)
	if err != nil {
		respondSecurityError(w, err)
		return
	}
	existing, err := h.service.ResolveTarget(r.Context(), targetIQN)
	if err != nil {
		respondSecurityError(w, err)
		return
	}
	binding := existing.Binding
	binding.Generation = req.Generation
	binding.Authentication = authPolicy
	impact, err := h.service.PreviewBinding(r.Context(), binding)
	if err != nil {
		respondSecurityError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"targets": impact})
}

func decodeISCSIBindingPut(r *http.Request) (iscsiBindingPutRequest, *domain.ISCSIAuthenticationPolicy, error) {
	var req iscsiBindingPutRequest
	if err := decodeRequiredJSONBody(r, &req); err != nil || req.Generation <= 0 || len(req.Auth) == 0 {
		return req, nil, domain.ErrInvalidInput
	}
	var authPolicy *domain.ISCSIAuthenticationPolicy
	if !bytes.Equal(bytes.TrimSpace(req.Auth), []byte("null")) {
		authPolicy = &domain.ISCSIAuthenticationPolicy{}
		if err := decodeISCSIJSON(req.Auth, authPolicy); err != nil {
			return req, nil, domain.ErrInvalidInput
		}
	}
	if authPolicy != nil && authPolicy.Validate() != nil {
		return req, nil, domain.ErrInvalidInput
	}
	return req, authPolicy, nil
}

func decodeISCSIJSON(payload []byte, output any) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return domain.ErrInvalidInput
	}
	return nil
}

func (h *iscsiSecurityHandler) handleCredentials(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		credentials, err := h.service.ListCredentials(r.Context())
		if err != nil {
			respondSecurityError(w, err)
			return
		}
		respondJSON(w, http.StatusOK, map[string]any{"credentials": credentialMetadataJSON(credentials)})
	case http.MethodPost:
		fields := map[string]json.RawMessage{}
		if err := decodeRequiredJSONBody(r, &fields); err != nil {
			respondSecurityError(w, err)
			return
		}
		allowed := map[string]bool{"credentialId": true, "label": true, "forwardUsername": true, "forwardSecret": true, "reverseUsername": true, "reverseSecret": true, "actor": true}
		for key := range fields {
			if !allowed[key] {
				respondSecurityError(w, domain.ErrInvalidInput)
				return
			}
		}
		values := make(map[string]string, len(fields))
		for _, name := range []string{"credentialId", "label", "forwardUsername", "forwardSecret", "reverseUsername", "reverseSecret", "actor"} {
			if raw, ok := fields[name]; ok {
				var value string
				if err := json.Unmarshal(raw, &value); err != nil {
					respondSecurityError(w, domain.ErrInvalidInput)
					return
				}
				values[name] = value
			}
		}
		if values["credentialId"] == "" || values["label"] == "" || values["forwardUsername"] == "" || values["forwardSecret"] == "" ||
			(values["reverseUsername"] == "") != (values["reverseSecret"] == "") {
			respondSecurityError(w, domain.ErrInvalidInput)
			return
		}
		metadata, err := h.service.CreateCredential(r.Context(), orchestration.ISCSICredentialInput{
			CredentialID: values["credentialId"], Label: values["label"],
			Secret: domain.ISCSISecret{Username: values["forwardUsername"], Secret: values["forwardSecret"], MutualUsername: values["reverseUsername"], MutualSecret: values["reverseSecret"]},
		}, values["actor"])
		if err != nil {
			respondSecurityError(w, err)
			return
		}
		respondJSON(w, http.StatusCreated, credentialMetadataJSON([]orchestration.ISCSICredentialMetadata{metadata})[0])
	default:
		respondError(w, http.StatusMethodNotAllowed, "method not allowed", nil)
	}
}

func (h *iscsiSecurityHandler) handleCredential(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		respondError(w, http.StatusMethodNotAllowed, "method not allowed", nil)
		return
	}
	if err := h.service.DeleteCredential(r.Context(), r.PathValue("id"), strings.TrimSpace(r.URL.Query().Get("actor"))); err != nil {
		respondSecurityError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func credentialMetadataJSON(credentials []orchestration.ISCSICredentialMetadata) []map[string]any {
	out := make([]map[string]any, 0, len(credentials))
	for _, credential := range credentials {
		out = append(out, map[string]any{
			"credentialId": credential.CredentialID, "label": credential.Label, "username": credential.Username,
			"mutualUsername": credential.MutualUsername, "version": credential.Version, "createdAt": credential.CreatedAt,
			"secretPresent": credential.SecretPresent,
		})
	}
	return out
}

func respondSecurityError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	message := "security operation failed"
	switch {
	case errors.Is(err, domain.ErrInvalidInput):
		status, message = http.StatusBadRequest, "invalid iSCSI security request"
	case errors.Is(err, domain.ErrNotFound):
		status, message = http.StatusNotFound, "security resource not found"
	case errors.Is(err, domain.ErrConflict), errors.Is(err, orchestration.ErrISCSISecurityBusy):
		status, message = http.StatusConflict, "iSCSI security resource is busy or conflicted"
	case errors.Is(err, orchestration.ErrISCSISecurityRuntimeUnknown), errors.Is(err, orchestration.ErrISCSISecurityHelperUnavailable), errors.Is(err, orchestration.ErrISCSISecretKeyMissing):
		status, message = http.StatusServiceUnavailable, "iSCSI security runtime is unavailable"
	}
	respondError(w, status, message, err)
}
