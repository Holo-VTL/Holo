package api

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
	"github.com/Holo-VTL/Holo/control-plane/internal/orchestration"
)

func (h *ResourcesHandler) ensureLibraryAutoPublications(ctx context.Context, libraryID string) error {
	if h.target == nil {
		return nil
	}
	libraryID = strings.TrimSpace(libraryID)
	if libraryID == "" {
		return domain.ErrInvalidInput
	}

	library, err := h.repo.FindLibrary(ctx, libraryID)
	if err != nil {
		return err
	}

	drives := make([]*domain.VirtualDrive, 0)
	for _, drive := range h.repo.ListDrives(ctx) {
		if drive != nil && strings.TrimSpace(drive.LibraryID) == libraryID {
			drives = append(drives, drive)
		}
	}
	if len(drives) == 0 {
		h.syncLocalMount("system")
		return nil
	}
	sort.Slice(drives, func(i, j int) bool {
		if drives[i].Slot == drives[j].Slot {
			return drives[i].DriveID < drives[j].DriveID
		}
		return drives[i].Slot < drives[j].Slot
	})

	cartridges := make([]*domain.VirtualCartridge, 0)
	for _, cartridge := range h.repo.ListCartridges(ctx) {
		if cartridge != nil && strings.TrimSpace(cartridge.LibraryID) == libraryID {
			cartridges = append(cartridges, cartridge)
		}
	}
	if len(cartridges) == 0 {
		h.syncLocalMount("system")
		return nil
	}
	sort.Slice(cartridges, func(i, j int) bool {
		if cartridges[i].Barcode == cartridges[j].Barcode {
			return cartridges[i].CartridgeID < cartridges[j].CartridgeID
		}
		return cartridges[i].Barcode < cartridges[j].Barcode
	})

	readyByIQN := make(map[string]*domain.TargetPublication)
	for _, publication := range h.target.ListPublications(ctx) {
		if publication == nil {
			continue
		}
		if publication.State != domain.PublicationReady && publication.State != domain.PublicationCreating {
			continue
		}
		readyByIQN[strings.TrimSpace(publication.TargetIQN)] = publication
	}

	published := make(map[string]struct{})
	firstDrive := drives[0]
	firstCartridge := cartridges[0]
	driveProfile := normalizeDeviceProfile(library.Vendor, library.DriveType, "drive")
	libraryIQN := strings.TrimSpace(library.IQN)
	if libraryIQN != "" {
		if existing := readyByIQN[libraryIQN]; existing == nil {
			if _, err := h.target.Publish(ctx, orchestration.PublishRequest{
				LibraryID:     libraryID,
				DriveID:       firstDrive.DriveID,
				CartridgeID:   firstCartridge.CartridgeID,
				TargetIQN:     libraryIQN,
				DeviceRole:    "changer",
				DeviceProfile: normalizeDeviceProfile(library.Vendor, library.LibraryType, "changer"),
				DriveProfile:  driveProfile,
				Actor:         "system",
				Auto:          true,
			}); err != nil && !errors.Is(err, domain.ErrConflict) && !errors.Is(err, orchestration.ErrISCSISecurityBusy) {
				return err
			}
		}
		published[libraryIQN] = struct{}{}
	}

	for idx, drive := range drives {
		if drive == nil {
			continue
		}
		driveIQN := strings.TrimSpace(drive.IQN)
		if driveIQN == "" {
			continue
		}
		if existing := readyByIQN[driveIQN]; existing == nil {
			cartridge := cartridges[idx%len(cartridges)]
			if _, err := h.target.Publish(ctx, orchestration.PublishRequest{
				LibraryID:     libraryID,
				DriveID:       drive.DriveID,
				CartridgeID:   cartridge.CartridgeID,
				TargetIQN:     driveIQN,
				DeviceRole:    "drive",
				DeviceProfile: driveProfile,
				DriveProfile:  driveProfile,
				Actor:         "system",
				Auto:          true,
			}); err != nil && !errors.Is(err, domain.ErrConflict) && !errors.Is(err, orchestration.ErrISCSISecurityBusy) {
				return err
			}
		}
		published[driveIQN] = struct{}{}
	}

	for iqn, publication := range readyByIQN {
		if publication == nil || strings.TrimSpace(publication.LibraryID) != libraryID {
			continue
		}
		if _, keep := published[iqn]; keep {
			continue
		}
		if _, err := h.target.Unpublish(ctx, publication.PublicationID, "system"); err != nil && !errors.Is(err, domain.ErrNotFound) {
			return err
		}
	}
	h.syncLocalMount("system")
	return nil
}

func (h *ResourcesHandler) ensureUpgradeRuntimePublications(ctx context.Context) error {
	if h.target == nil {
		return nil
	}
	for _, publication := range h.target.ListPublications(ctx) {
		if publication == nil {
			continue
		}
		if publication.State == domain.PublicationReady || publication.State == domain.PublicationCreating {
			return nil
		}
	}
	for _, library := range h.repo.ListLibraries(ctx) {
		if library == nil {
			continue
		}
		if err := h.ensureLibraryAutoPublications(ctx, library.LibraryID); err != nil {
			return err
		}
	}
	return nil
}

func normalizeDeviceProfile(vendor, model, fallback string) string {
	vendorToken := sanitizeProfileToken(vendor)
	modelToken := sanitizeProfileToken(model)
	if mapped := profileAliasFor(modelToken); mapped != "" {
		return mapped
	}
	if vendorToken == "" && modelToken == "" {
		return fallback
	}
	if vendorToken == "" {
		return modelToken
	}
	if modelToken == "" {
		return vendorToken
	}
	return vendorToken + "-" + modelToken
}

func profileAliasFor(modelToken string) string {
	switch modelToken {
	case "ibm-ts2230", "ibm-ult3580-td3", "ult3580-td3":
		return "ibm-ult3580-td3"
	case "ibm-ts2240", "ibm-ult3580-td4", "ult3580-td4":
		return "ibm-ult3580-td4"
	case "ibm-ts2250", "ibm-ult3580-td5", "ult3580-td5":
		return "ibm-ult3580-td5"
	case "ibm-ts2260", "ibm-ult3580-td6", "ult3580-td6":
		return "ibm-ult3580-td6"
	case "ibm-ts2270", "ibm-ult3580-td7", "ult3580-td7":
		return "ibm-ult3580-td7"
	case "ibm-ts2280", "ibm-ult3580-td8", "ult3580-td8":
		return "ibm-ult3580-td8"
	case "ibm-ts2290", "ibm-ult3580-td9", "ult3580-td9":
		return "ibm-ult3580-td9"
	case "ibm-lto-10-tape-drive", "ibm-ult3580-tda", "ult3580-tda":
		return "ibm-ult3580-tda"
	case "hp-ultrium-920", "hp-ultrium-960", "hp-ultrium-3-scsi", "ultrium-3-scsi":
		return "hp-ultrium-3-scsi"
	case "hp-ultrium-1760", "hp-ultrium-1840", "hp-ultrium-4-scsi", "ultrium-4-scsi":
		return "hp-ultrium-4-scsi"
	case "hp-ultrium-3000", "hp-ultrium-3280", "hp-ultrium-5-scsi", "ultrium-5-scsi":
		return "hp-ultrium-5-scsi"
	case "hp-storeever-ultrium-6250", "hp-storeever-ultrium-6650", "hp-ultrium-6-scsi", "ultrium-6-scsi":
		return "hp-ultrium-6-scsi"
	case "hpe-storeever-lto-7-ultrium-15000", "hpe-ultrium-7-scsi", "ultrium-7-scsi":
		return "hpe-ultrium-7-scsi"
	case "hpe-storeever-lto-8-ultrium-30750", "hpe-ultrium-8-scsi", "ultrium-8-scsi":
		return "hpe-ultrium-8-scsi"
	case "hpe-storeever-lto-9-ultrium-45000", "hpe-ultrium-9-scsi", "ultrium-9-scsi":
		return "hpe-ultrium-9-scsi"
	case "quantum-lto-3-tape-drive":
		return "quantum-ultrium-td3"
	case "quantum-lto-4-tape-drive":
		return "quantum-ultrium-td4"
	case "quantum-lto-5-tape-drive":
		return "quantum-ultrium-td5"
	case "quantum-lto-6-tape-drive":
		return "quantum-ultrium-td6"
	case "quantum-lto-7-tape-drive":
		return "quantum-ultrium-td7"
	case "quantum-lto-8-tape-drive":
		return "quantum-ultrium-td8"
	case "quantum-lto-9-tape-drive":
		return "quantum-ultrium-td9"
	case "quantum-lto-10-tape-drive":
		return "quantum-ultrium-tda"
	case "storagetek-t9840a", "t9840a":
		return "stk-t9840a"
	case "storagetek-t9840b", "t9840b":
		return "stk-t9840b"
	case "storagetek-t9840c", "t9840c":
		return "stk-t9840c"
	case "storagetek-t9840d", "t9840d":
		return "stk-t9840d"
	case "storagetek-t9940a", "t9940a":
		return "stk-t9940a"
	case "storagetek-t9940b", "t9940b":
		return "stk-t9940b"
	case "storagetek-t10000a", "t10000a":
		return "stk-t10000a"
	case "storagetek-t10000b", "t10000b":
		return "stk-t10000b"
	case "storagetek-t10000c", "t10000c":
		return "stk-t10000c"
	case "storagetek-t10000d", "t10000d":
		return "stk-t10000d"
	case "ibm-3592-j1a", "03592j1a":
		return "ibm-03592j1a"
	case "ibm-3592-e05", "03592e05":
		return "ibm-03592e05"
	case "ibm-3592-e06", "03592e06":
		return "ibm-03592e06"
	case "ibm-ts3100", "ibm-ts3200", "3573-tl":
		return "ibm-3573-tl"
	case "ibm-ts3310":
		return "ibm-ts3310"
	case "ibm-ts3500", "03584l32":
		return "ibm-03584l32"
	case "ibm-ts4300":
		return "ibm-ts4300"
	case "ibm-ts4500":
		return "ibm-ts4500"
	case "ibm-diamondback":
		return "ibm-diamondback"
	case "hp-esl9000-series":
		return "hp-esl9000-series"
	case "hp-esl-e-series":
		return "hp-esl-e-series"
	case "hp-eml-e-series":
		return "hp-eml-e-series"
	case "hp-msl-g3-series":
		return "hp-msl-g3-series"
	case "hp-hpe-msl2024", "hphpe-msl2024", "hphpe-hphpe-msl2024":
		return "hp-msl2024"
	case "hp-hpe-msl4048", "hphpe-msl4048", "hphpe-hphpe-msl4048":
		return "hp-msl4048"
	case "hp-hpe-msl8096", "hphpe-msl8096", "hphpe-hphpe-msl8096":
		return "hp-msl8096"
	case "hp-msl6000-series":
		return "hp-msl6000-series"
	case "hpe-msl3040":
		return "hpe-msl3040"
	case "hpe-msl6480":
		return "hpe-msl6480"
	case "adic-scalar-24":
		return "adic-scalar-24"
	case "adic-scalar-100":
		return "adic-scalar-100"
	case "quantum-scalar-i500", "adic-scalar-i500":
		return "adic-scalar-i500"
	case "adic-scalar-i2000":
		return "adic-scalar-i2000"
	case "quantum-scalar-i40":
		return "quantum-scalar-i40"
	case "quantum-scalar-i80":
		return "quantum-scalar-i80"
	case "quantum-scalar-i6000":
		return "quantum-scalar-i6000"
	case "quantum-scalar-i3":
		return "quantum-scalar-i3"
	case "quantum-scalar-i6":
		return "quantum-scalar-i6"
	case "quantum-superloader-3":
		return "quantum-superloader-3"
	case "dell-powervault-tl1000":
		return "dell-tl1000"
	case "dell-powervault-tl2000":
		return "dell-tl2000"
	case "dell-powervault-tl4000":
		return "dell-tl4000"
	case "dell-emc-ml3":
		return "dell-ml3"
	case "dell-powervault-ml6000":
		return "dell-ml6000"
	case "storagetek-l20":
		return "stk-l20"
	case "storagetek-l80":
		return "stk-l80"
	case "storagetek-l700":
		return "stk-l700"
	case "storagetek-sl150":
		return "stk-sl150"
	case "storagetek-sl3000":
		return "stk-sl3000"
	case "storagetek-sl4000":
		return "stk-sl4000"
	case "storagetek-sl8500":
		return "stk-sl8500"
	case "spectra-t50e":
		return "spectra-t50e"
	case "spectra-t120":
		return "spectra-t120"
	case "spectra-t200":
		return "spectra-t200"
	case "spectra-t380":
		return "spectra-t380"
	case "spectra-t680":
		return "spectra-t680"
	case "spectra-t950":
		return "spectra-t950"
	case "spectra-t950v":
		return "spectra-t950v"
	case "spectra-tfinity":
		return "spectra-tfinity"
	case "spectra-tfinity-exascale":
		return "spectra-tfinity-exascale"
	case "spectra-stack":
		return "spectra-stack"
	case "spectra-python":
		return "spectra-python"
	case "overland-neo-series":
		return "overland-neo-series"
	case "overland-flexstor-ii":
		return "overland-flexstor-ii"
	case "overland-neoxl-multistak":
		return "overland-neoxl-multistak"
	case "tandberg-neos-t24":
		return "tandberg-neos-t24"
	case "tandberg-neoxl-40":
		return "tandberg-neoxl-40"
	case "tandberg-neoxl-80":
		return "tandberg-neoxl-80"
	default:
		return ""
	}
}

func sanitizeProfileToken(raw string) string {
	raw = strings.ToLower(strings.TrimSpace(raw))
	var b strings.Builder
	for _, ch := range raw {
		if (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') {
			b.WriteRune(ch)
			continue
		}
		if ch == '-' || ch == '_' || ch == '.' || ch == ' ' {
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}
