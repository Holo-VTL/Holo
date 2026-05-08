package api

import "github.com/Holo-VTL/Holo/control-plane/internal/domain"

func validateManagementID(value string) error {
	return domain.ValidateManagementID(value)
}

func validateManagementLabel(value string, required bool) error {
	return domain.ValidateManagementLabel(value, required)
}

func validateProfileToken(value string) error {
	return domain.ValidateProfileToken(value)
}
