package repo

import (
	"context"

	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
)

type ISCSISecurityRepository interface {
	SaveBinding(context.Context, domain.ISCSISecurityBinding) error
	FindBinding(context.Context, domain.SecurityScope, string) (domain.ISCSISecurityBinding, error)
	RegisterTarget(context.Context, string, string, string, string) error
	SetTargetOffline(context.Context, string, bool) error
	FindTarget(context.Context, string) (domain.ISCSISecurityBinding, error)
	ListTargets(context.Context) ([]domain.ISCSISecurityBinding, error)
	CreateCredential(context.Context, domain.ISCSICredential) error
	FindCredential(context.Context, string) (domain.ISCSICredential, error)
	ListCredentials(context.Context) ([]domain.ISCSICredential, error)
	DeleteCredential(context.Context, string) error
	AppendSnapshot(context.Context, domain.ISCSISecuritySnapshot) error
	ListSnapshots(context.Context, string, string) ([]domain.ISCSISecuritySnapshot, error)
}
