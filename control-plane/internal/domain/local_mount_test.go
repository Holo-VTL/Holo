package domain

import (
	"errors"
	"testing"
)

func TestLocalLoopbackLibraryMappingValidatesPersistentIdentity(t *testing.T) {
	mapping := LocalLoopbackLibraryMapping{
		LibraryID: "library-a",
		TargetNAA: "naa.50014056b18af0f5",
		NexusNAA:  "naa.5001405db2f4505b",
		TPGTag:    1,
	}
	if err := mapping.Validate(); err != nil {
		t.Fatalf("valid library mapping rejected: %v", err)
	}

	for name, mutate := range map[string]func(*LocalLoopbackLibraryMapping){
		"invalid library id":    func(value *LocalLoopbackLibraryMapping) { value.LibraryID = "../library" },
		"invalid target NAA":    func(value *LocalLoopbackLibraryMapping) { value.TargetNAA = "/dev/sda" },
		"same target and nexus": func(value *LocalLoopbackLibraryMapping) { value.NexusNAA = value.TargetNAA },
		"unsupported TPG":       func(value *LocalLoopbackLibraryMapping) { value.TPGTag = 2 },
	} {
		t.Run(name, func(t *testing.T) {
			invalid := mapping
			mutate(&invalid)
			if err := invalid.Validate(); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("expected ErrInvalidInput, got %v", err)
			}
		})
	}
}

func TestLocalLoopbackDeviceMappingEnforcesRoleAndLUN(t *testing.T) {
	valid := LocalLoopbackDeviceMapping{
		DeviceKey:   "changer:library-a",
		LibraryID:   "library-a",
		Kind:        LocalDeviceKindChanger,
		LUNIndex:    0,
		IdentityRef: "changer-identity-a",
		BackendRef:  "holo_backstore_a",
		State:       LocalMappingStateActive,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid changer mapping rejected: %v", err)
	}

	invalidCases := map[string]LocalLoopbackDeviceMapping{
		"changer must use its library key": func() LocalLoopbackDeviceMapping {
			v := valid
			v.DeviceKey = "drive:drive-a"
			return v
		}(),
		"changer has no drive id": func() LocalLoopbackDeviceMapping {
			v := valid
			v.DriveID = "drive-a"
			return v
		}(),
		"changer uses LUN zero": func() LocalLoopbackDeviceMapping {
			v := valid
			v.LUNIndex = 1
			return v
		}(),
		"drive requires drive key and nonzero LUN": {
			DeviceKey: "drive:drive-a", LibraryID: "library-a", Kind: LocalDeviceKindDrive,
			DriveID: "drive-a", LUNIndex: 0, IdentityRef: "drive-identity-a",
			BackendRef: "holo_backstore_b", State: LocalMappingStateActive,
		},
		"LUN upper bound": {
			DeviceKey: "drive:drive-a", LibraryID: "library-a", Kind: LocalDeviceKindDrive,
			DriveID: "drive-a", LUNIndex: 65536, IdentityRef: "drive-identity-a",
			BackendRef: "holo_backstore_b", State: LocalMappingStateActive,
		},
		"invalid cleanup state": {
			DeviceKey: "drive:drive-a", LibraryID: "library-a", Kind: LocalDeviceKindDrive,
			DriveID: "drive-a", LUNIndex: 1, IdentityRef: "drive-identity-a",
			BackendRef: "holo_backstore_b", State: "removing",
		},
	}
	for name, mapping := range invalidCases {
		t.Run(name, func(t *testing.T) {
			if err := mapping.Validate(); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("expected ErrInvalidInput, got %v", err)
			}
		})
	}

	drive := LocalLoopbackDeviceMapping{
		DeviceKey: "drive:drive-a", LibraryID: "library-a", Kind: LocalDeviceKindDrive,
		DriveID: "drive-a", LUNIndex: 65535, IdentityRef: "drive-identity-a",
		BackendRef: "holo_backstore_b", State: LocalMappingStateCleanupPending,
	}
	if err := drive.Validate(); err != nil {
		t.Fatalf("valid upper-bound drive mapping rejected: %v", err)
	}
}
