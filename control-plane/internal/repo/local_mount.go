package repo

import (
	"context"

	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
)

type LocalMountRepository interface {
	Enabled(context.Context) (bool, error)
	SetEnabled(context.Context, bool) error
	SaveLibraryMapping(context.Context, domain.LocalLoopbackLibraryMapping) error
	ListLibraryMappings(context.Context) ([]domain.LocalLoopbackLibraryMapping, error)
	DeleteLibraryMapping(context.Context, string) error
	SaveDeviceMapping(context.Context, domain.LocalLoopbackDeviceMapping) error
	ListDeviceMappings(context.Context, string) ([]domain.LocalLoopbackDeviceMapping, error)
	MarkDeviceCleanupPending(context.Context, string) error
	MarkDeviceActive(context.Context, string) error
	MarkDeviceInactive(context.Context, string) error
	DeleteDeviceMapping(context.Context, string) error
}
