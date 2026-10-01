package orchestration

import (
	"context"
	"net"

	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
)

const defaultISCSISecurityHelperPath = "/opt/holo/bin/holo-iscsi-security-helper"

func NewDefaultISCSISecurityHelper(useSudo bool) *ISCSISecurityHelper {
	return NewISCSISecurityHelper(defaultISCSISecurityHelperPath, useSudo)
}

func protectedTargetRequest(publication *domain.TargetPublication, security ISCSIResolvedPublicationSecurity, backstoreName, backstoreType, address string, port int) (map[string]any, error) {
	if publication == nil || domain.ValidateTargetIQN(publication.TargetIQN) != nil || len(backstoreName) < 6 || backstoreName[:5] != "holo_" ||
		(backstoreType != "fileio" && backstoreType != "user:holo") {
		return nil, domain.ErrInvalidInput
	}
	ip := net.ParseIP(address)
	if ip == nil || ip.To4() == nil || !ip.IsGlobalUnicast() || ip.IsLoopback() || ip.IsMulticast() || ip.IsLinkLocalUnicast() ||
		port < 1024 || port > 65535 {
		return nil, domain.ErrInvalidInput
	}
	auth := map[string]any{"mode": string(security.Policy.Authentication.Mode)}
	if security.Policy.Authentication.Mode == domain.ISCSIAuthNone && security.Policy.Authentication.RestrictInitiators {
		auth["restrictInitiators"] = true
	}
	switch security.Policy.Authentication.Mode {
	case domain.ISCSIAuthNone:
	case domain.ISCSIAuthCHAP, domain.ISCSIAuthMutualCHAP:
		if security.Credential == nil {
			return nil, ErrISCSISecretKeyMissing
		}
		auth["username"] = security.Credential.Username
		auth["secret"] = security.Credential.Secret
		if security.Policy.Authentication.Mode == domain.ISCSIAuthMutualCHAP {
			if security.Credential.MutualSecret == "" {
				return nil, domain.ErrInvalidState
			}
			auth["mutualUsername"] = security.Credential.MutualUsername
			auth["mutualSecret"] = security.Credential.MutualSecret
		}
	default:
		return nil, domain.ErrInvalidInput
	}
	return map[string]any{
		"version": 1, "operation": "create-target-protected", "targetIQN": publication.TargetIQN,
		"backstoreName": backstoreName, "backstoreType": backstoreType,
		"endpoint": map[string]any{"address": ip.To4().String(), "port": port},
		"auth":     auth, "initiators": append([]string(nil), security.Policy.Authentication.Initiators...),
	}, nil
}

func runProtectedTargetHelper(ctx context.Context, helper *ISCSISecurityHelper, publication *domain.TargetPublication, security ISCSIResolvedPublicationSecurity, backstoreName, backstoreType, address string, port int) error {
	request, err := protectedTargetRequest(publication, security, backstoreName, backstoreType, address, port)
	if err != nil {
		return err
	}
	response, err := helper.Call(ctx, request)
	if err != nil || response.Ready == nil || !*response.Ready {
		return ErrISCSISecurityHelperUnavailable
	}
	return nil
}
