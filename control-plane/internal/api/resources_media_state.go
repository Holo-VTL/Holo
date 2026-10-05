package api

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
	"github.com/Holo-VTL/Holo/control-plane/internal/storageutil"
)

func (h *ResourcesHandler) syncLibrarySlotsToSharedState(ctx context.Context, libraryID string) error {
	if strings.TrimSpace(libraryID) == "" {
		return domain.ErrInvalidInput
	}

	library, err := h.repo.FindLibrary(ctx, libraryID)
	if err != nil {
		return err
	}

	if err := h.repairLegacyAssignedSlotsForLibrary(ctx, library); err != nil {
		return err
	}
	cartridges := h.repo.ListCartridges(ctx)
	filtered := make([]*domain.VirtualCartridge, 0)
	exported := make([]*domain.VirtualCartridge, 0)
	activeLabels := make(map[string]struct{})
	for _, cartridge := range cartridges {
		if cartridge == nil || cartridge.LibraryID != libraryID {
			continue
		}
		if cartridge.LifecycleState == domain.CartridgeExported {
			exported = append(exported, cartridge)
			continue
		}
		if cartridge.LifecycleState != domain.CartridgeMounted {
			filtered = append(filtered, cartridge)
			activeLabels[cartridgeSharedLabel(cartridge)] = struct{}{}
		}
	}
	sort.Slice(filtered, func(i, j int) bool {
		return filtered[i].CartridgeID < filtered[j].CartridgeID
	})
	sort.Slice(exported, func(i, j int) bool {
		if !exported[i].UpdatedAt.Equal(exported[j].UpdatedAt) {
			return exported[i].UpdatedAt.After(exported[j].UpdatedAt)
		}
		return exported[i].CartridgeID < exported[j].CartridgeID
	})
	slotCount := library.SlotCount
	if slotCount <= 0 {
		slotCount = 1
	}

	drives := h.repo.ListDrives(ctx)
	driveIDs := make([]string, 0)
	for _, drive := range drives {
		if drive != nil && drive.LibraryID == libraryID {
			driveIDs = append(driveIDs, drive.DriveID)
		}
	}
	if len(driveIDs) == 0 {
		return nil
	}

	dir := mediaStateDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	existingLabels := librarySlotLabelsSnapshot(ctx, h.repo, libraryID, slotCount)
	labels := stableSlotLabels(slotCount, librarySlotStart(library), existingLabels, filtered, activeLabels)
	removeLoadedLabelsFromSlots(labels, readLoadedLabels(libraryID, driveIDs))
	slotPayload := strings.Join(labels, "\n") + "\n"
	ieCount := library.IEPortCount
	if ieCount <= 0 {
		ieCount = 1
	}
	ieLabels := exportedIELabels(ieCount, nil)
	iePayload := strings.Join(ieLabels, "\n") + "\n"
	vaultLabels := exportedVaultLabels(exported)
	vaultPayload := ""
	if len(vaultLabels) > 0 {
		vaultPayload = strings.Join(vaultLabels, "\n") + "\n"
	}
	for _, driveID := range driveIDs {
		stateKey := storageutil.MediaStateKey(libraryID, driveID)
		basePath := filepath.Join(dir, sanitizeStateID(stateKey))
		if err := writeAtomicText(basePath+".slots", slotPayload); err != nil {
			return err
		}
		if err := writeAtomicText(basePath+".ie", iePayload); err != nil {
			return err
		}
		if err := writeAtomicText(basePath+".vault", vaultPayload); err != nil {
			return err
		}
	}
	for _, cartridge := range cartridges {
		if cartridge == nil || cartridge.LibraryID != libraryID {
			continue
		}
		if err := writeCartridgeMetadata(cartridge); err != nil {
			return err
		}
	}
	return nil
}

func (h *ResourcesHandler) reconcileMediaState(ctx context.Context) error {
	drives := h.repo.ListDrives(ctx)
	cartridges := h.repo.ListCartridges(ctx)
	cartridgeByLabel := make(map[string]*domain.VirtualCartridge)
	for _, cartridge := range cartridges {
		if cartridge == nil {
			continue
		}
		cartridgeByLabel[strings.ToUpper(strings.TrimSpace(cartridge.CartridgeID))] = cartridge
		cartridgeByLabel[strings.ToUpper(strings.TrimSpace(cartridge.Barcode))] = cartridge
	}

	mounted := make(map[string]struct{})
	exported := make(map[string]struct{})
	for _, drive := range drives {
		if drive == nil {
			continue
		}
		label, err := readDriveMediaState(drive.LibraryID, drive.DriveID)
		if err != nil {
			return err
		}
		mountedID := strings.TrimSpace(label)
		if cartridge := cartridgeByLabel[strings.ToUpper(mountedID)]; cartridge != nil {
			mountedID = cartridge.CartridgeID
		}

		changed := false
		if mountedID == "" {
			if drive.MountState != domain.MountEmpty || strings.TrimSpace(drive.MountedCartridgeID) != "" {
				drive.MountState = domain.MountEmpty
				drive.MountedCartridgeID = ""
				drive.UpdatedAt = time.Now().UTC()
				changed = true
			}
		} else {
			mounted[mountedID] = struct{}{}
			if drive.MountState != domain.MountLoaded || strings.TrimSpace(drive.MountedCartridgeID) != mountedID {
				drive.MountState = domain.MountLoaded
				drive.MountedCartridgeID = mountedID
				drive.UpdatedAt = time.Now().UTC()
				changed = true
			}
		}
		if changed {
			if err := h.repo.SaveDrive(ctx, drive); err != nil {
				return err
			}
		}

		for _, label := range readExistingIELabels(drive.LibraryID, drive.DriveID) {
			cartridge := cartridgeByLabel[strings.ToUpper(strings.TrimSpace(label))]
			if cartridge != nil {
				exported[cartridge.CartridgeID] = struct{}{}
			}
		}
		for _, label := range readExistingVaultLabels(drive.LibraryID, drive.DriveID) {
			cartridge := cartridgeByLabel[strings.ToUpper(strings.TrimSpace(label))]
			if cartridge != nil {
				exported[cartridge.CartridgeID] = struct{}{}
			}
		}
	}

	if err := h.repairLegacyAssignedSlots(ctx, cartridges, mounted, exported); err != nil {
		return err
	}

	for _, cartridge := range cartridges {
		if cartridge == nil || cartridge.LifecycleState == domain.CartridgeRetired {
			continue
		}
		if meta, err := readCartridgeMetadata(cartridge); err != nil {
			return err
		} else if meta != nil {
			changed := false
			if meta.CapacityBytes > 0 && cartridge.CapacityBytes != meta.CapacityBytes {
				cartridge.CapacityBytes = meta.CapacityBytes
				changed = true
			}
			if meta.UsedBytes >= 0 && cartridge.UsedBytes != meta.UsedBytes {
				cartridge.UsedBytes = meta.UsedBytes
				changed = true
			}
			if changed {
				cartridge.UpdatedAt = time.Now().UTC()
			}
		}
		_, isMounted := mounted[cartridge.CartridgeID]
		_, isExported := exported[cartridge.CartridgeID]
		next := domain.CartridgeAvailable
		if isMounted {
			next = domain.CartridgeMounted
		} else if isExported {
			next = domain.CartridgeExported
		}
		if cartridge.LifecycleState != next {
			cartridge.LifecycleState = next
			cartridge.UpdatedAt = time.Now().UTC()
		}
		if err := h.repo.SaveCartridge(ctx, cartridge); err != nil {
			return err
		}
	}
	return h.syncPoolUsageForCartridges(ctx, h.repo.ListCartridges(ctx))
}

func (h *ResourcesHandler) repairLegacyAssignedSlotsForLibrary(ctx context.Context, library *domain.VirtualLibrary) error {
	if library == nil {
		return nil
	}
	libraryID := strings.TrimSpace(library.LibraryID)
	cartridges := make([]*domain.VirtualCartridge, 0)
	for _, cartridge := range h.repo.ListCartridges(ctx) {
		if cartridge != nil && strings.TrimSpace(cartridge.LibraryID) == libraryID {
			cartridges = append(cartridges, cartridge)
		}
	}
	return h.repairLegacyAssignedSlots(ctx, cartridges, nil, nil)
}

// repairLegacyAssignedSlots backfills AssignedSlotAddress from the shared
// changer inventory for cartridges created before slot assignment existed.
func (h *ResourcesHandler) repairLegacyAssignedSlots(ctx context.Context, cartridges []*domain.VirtualCartridge, mounted, exported map[string]struct{}) error {
	occupiedByLibrary := make(map[string]map[int]string)
	libraryCache := make(map[string]*domain.VirtualLibrary)
	labelSlotsByLibrary := make(map[string]map[string]int)

	for _, cartridge := range cartridges {
		if cartridge == nil || cartridge.AssignedSlotAddress == nil {
			continue
		}
		libraryID := strings.TrimSpace(cartridge.LibraryID)
		if libraryID == "" || cartridge.LifecycleState == domain.CartridgeExported || cartridge.LifecycleState == domain.CartridgeRetired {
			continue
		}
		occupied := occupiedByLibrary[libraryID]
		if occupied == nil {
			occupied = make(map[int]string)
			occupiedByLibrary[libraryID] = occupied
		}
		occupied[*cartridge.AssignedSlotAddress] = cartridge.CartridgeID
	}

	for _, cartridge := range cartridges {
		if cartridge == nil || cartridge.AssignedSlotAddress != nil || cartridge.LifecycleState == domain.CartridgeMounted || cartridge.LifecycleState == domain.CartridgeExported || cartridge.LifecycleState == domain.CartridgeRetired {
			continue
		}
		if _, ok := mounted[cartridge.CartridgeID]; ok {
			continue
		}
		if _, ok := exported[cartridge.CartridgeID]; ok {
			continue
		}
		libraryID := strings.TrimSpace(cartridge.LibraryID)
		if libraryID == "" {
			continue
		}
		library := libraryCache[libraryID]
		if library == nil {
			found, err := h.repo.FindLibrary(ctx, libraryID)
			if err != nil {
				continue
			}
			library = found
			libraryCache[libraryID] = found
		}
		labelSlots := labelSlotsByLibrary[libraryID]
		if labelSlots == nil {
			labelSlots = h.libraryLabelSlots(ctx, library)
			labelSlotsByLibrary[libraryID] = labelSlots
		}
		address, ok := labelSlots[strings.ToUpper(strings.TrimSpace(cartridge.CartridgeID))]
		if !ok {
			address, ok = labelSlots[strings.ToUpper(strings.TrimSpace(cartridge.Barcode))]
		}
		if !ok {
			continue
		}
		occupied := occupiedByLibrary[libraryID]
		if occupied == nil {
			occupied = make(map[int]string)
			occupiedByLibrary[libraryID] = occupied
		}
		if existing := occupied[address]; existing != "" && existing != cartridge.CartridgeID {
			log.Printf("legacy slot assignment repair skipped library=%s cartridge=%s slot=%d occupiedBy=%s", libraryID, cartridge.CartridgeID, address, existing)
			continue
		}
		cartridge.AssignedSlotAddress = &address
		cartridge.CurrentElementAddress = &address
		cartridge.UpdatedAt = time.Now().UTC()
		if err := h.repo.SaveCartridge(ctx, cartridge); err != nil {
			return err
		}
		occupied[address] = cartridge.CartridgeID
		h.emitCartridgeAudit(ctx, "system", "cartridge_slot_repaired", cartridge, "success", map[string]any{
			"assignedSlot": address,
			"reason":       "legacy_unassigned",
		})
		log.Printf("legacy slot assignment repaired library=%s cartridge=%s assignedSlot=%d", libraryID, cartridge.CartridgeID, address)
	}
	return nil
}

func (h *ResourcesHandler) ReconcileMediaState(ctx context.Context) error {
	return h.reconcileMediaState(ctx)
}

func (h *ResourcesHandler) annotateCartridgeElementAddresses(ctx context.Context, cartridges []*domain.VirtualCartridge) {
	if len(cartridges) == 0 {
		return
	}
	cartridgeByLibraryLabel := make(map[string]map[string]*domain.VirtualCartridge)
	for _, cartridge := range cartridges {
		if cartridge == nil {
			continue
		}
		cartridge.CurrentElementAddress = nil
		libraryID := strings.TrimSpace(cartridge.LibraryID)
		if libraryID == "" {
			continue
		}
		labels := cartridgeByLibraryLabel[libraryID]
		if labels == nil {
			labels = make(map[string]*domain.VirtualCartridge)
			cartridgeByLibraryLabel[libraryID] = labels
		}
		for _, label := range []string{cartridge.CartridgeID, cartridge.Barcode} {
			key := strings.ToUpper(strings.TrimSpace(label))
			if key != "" {
				labels[key] = cartridge
			}
		}
	}
	for libraryID, cartridgeByLabel := range cartridgeByLibraryLabel {
		library, err := h.repo.FindLibrary(ctx, libraryID)
		if err != nil {
			continue
		}
		slotStart := library.SlotStartAddress
		if slotStart <= 0 {
			slotStart = 1
		}
		for _, label := range readLibrarySlotLabels(ctx, h.repo, libraryID) {
			key := strings.ToUpper(strings.TrimSpace(label.label))
			if key == "" {
				continue
			}
			if cartridge := cartridgeByLabel[key]; cartridge != nil && cartridge.LifecycleState != domain.CartridgeMounted && cartridge.LifecycleState != domain.CartridgeExported {
				address := slotStart + label.index
				cartridge.CurrentElementAddress = &address
			}
		}
	}
}

type slotLabel struct {
	index int
	label string
}

func readLibrarySlotLabels(ctx context.Context, repo coreResourcesRepo, libraryID string) []slotLabel {
	drives := repo.ListDrives(ctx)
	sort.Slice(drives, func(i, j int) bool {
		if drives[i] == nil {
			return false
		}
		if drives[j] == nil {
			return true
		}
		return drives[i].DriveID < drives[j].DriveID
	})
	var selectedDriveID string
	var selectedModTime time.Time
	var selectedLabels []string
	selectedHasInventory := false
	for _, drive := range drives {
		if drive == nil || drive.LibraryID != libraryID {
			continue
		}
		labels, modTime, ok := readExistingSlotLabelsWithModTime(libraryID, drive.DriveID)
		if !ok {
			continue
		}
		if len(labels) == 0 {
			continue
		}
		hasInventory := slotLabelsHaveInventory(labels)
		if selectedLabels == nil ||
			(hasInventory && !selectedHasInventory) ||
			(hasInventory == selectedHasInventory && (modTime.After(selectedModTime) || (modTime.Equal(selectedModTime) && drive.DriveID < selectedDriveID))) {
			selectedDriveID = drive.DriveID
			selectedModTime = modTime
			selectedLabels = labels
			selectedHasInventory = hasInventory
		}
	}
	out := make([]slotLabel, 0, len(selectedLabels))
	for idx, label := range selectedLabels {
		out = append(out, slotLabel{index: idx, label: label})
	}
	return out
}

func slotLabelsHaveInventory(labels []string) bool {
	for _, label := range labels {
		label = strings.TrimSpace(label)
		if label != "" && label != "-" {
			return true
		}
	}
	return false
}

func librarySlotLabelsSnapshot(ctx context.Context, repo coreResourcesRepo, libraryID string, slotCount int) []string {
	slotLabels := readLibrarySlotLabels(ctx, repo, libraryID)
	if len(slotLabels) == 0 {
		return nil
	}
	labels := make([]string, slotCount)
	for _, slotLabel := range slotLabels {
		if slotLabel.index < 0 || slotLabel.index >= len(labels) {
			continue
		}
		labels[slotLabel.index] = slotLabel.label
	}
	return labels
}

func readExistingSlotLabelsWithModTime(libraryID, driveID string) ([]string, time.Time, bool) {
	stateKey := storageutil.MediaStateKey(libraryID, driveID)
	path := filepath.Join(mediaStateDir(), sanitizeStateID(stateKey)+".slots")
	info, err := os.Stat(path)
	if err != nil {
		return nil, time.Time{}, false
	}
	return readExistingSlotLabels(libraryID, driveID), info.ModTime(), true
}

func (h *ResourcesHandler) syncPoolUsage(ctx context.Context, poolIDs ...string) error {
	return h.syncPoolUsageForCartridges(ctx, h.repo.ListCartridges(ctx), poolIDs...)
}

func (h *ResourcesHandler) syncPoolUsageForCartridges(ctx context.Context, cartridges []*domain.VirtualCartridge, poolIDs ...string) error {
	if h.storage == nil {
		return nil
	}
	usedByPool := make(map[string]int64)
	targetPools := make(map[string]struct{})
	for _, raw := range poolIDs {
		poolID := strings.TrimSpace(raw)
		if poolID != "" {
			targetPools[poolID] = struct{}{}
		}
	}
	for _, cartridge := range cartridges {
		if cartridge == nil || cartridge.LifecycleState == domain.CartridgeRetired {
			continue
		}
		poolID := strings.TrimSpace(cartridge.PoolID)
		if poolID == "" {
			continue
		}
		if cartridge.UsedBytes > 0 {
			const maxInt64 = int64(1<<63 - 1)
			if usedByPool[poolID] > maxInt64-cartridge.UsedBytes {
				usedByPool[poolID] = maxInt64
			} else {
				usedByPool[poolID] += cartridge.UsedBytes
			}
		}
		targetPools[poolID] = struct{}{}
	}
	for poolID := range targetPools {
		if err := h.storage.ReconcilePoolUsedBytes(ctx, poolID, usedByPool[poolID]); err != nil {
			return err
		}
	}
	return nil
}

func cartridgeSharedLabel(cartridge *domain.VirtualCartridge) string {
	if cartridge == nil {
		return ""
	}
	label := strings.TrimSpace(cartridge.Barcode)
	if label == "" {
		label = strings.TrimSpace(cartridge.CartridgeID)
	}
	return label
}

func readExistingSlotLabels(libraryID, driveID string) []string {
	stateKey := storageutil.MediaStateKey(libraryID, driveID)
	path := filepath.Join(mediaStateDir(), sanitizeStateID(stateKey)+".slots")
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	labels := make([]string, len(lines))
	for idx, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" && trimmed != "-" {
			labels[idx] = trimmed
		}
	}
	return labels
}

func readLoadedLabels(libraryID string, driveIDs []string) map[string]struct{} {
	loaded := make(map[string]struct{})
	for _, driveID := range driveIDs {
		stateKey := storageutil.MediaStateKey(libraryID, driveID)
		path := filepath.Join(mediaStateDir(), sanitizeStateID(stateKey)+".state")
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(raw), "\n") {
			key, value, ok := strings.Cut(line, "=")
			if !ok || strings.TrimSpace(key) != "cartridge" {
				continue
			}
			label := strings.TrimSpace(value)
			if label != "" {
				loaded[label] = struct{}{}
			}
		}
	}
	return loaded
}

func removeLoadedLabelsFromSlots(labels []string, loaded map[string]struct{}) {
	if len(loaded) == 0 {
		return
	}
	for idx, label := range labels {
		if _, ok := loaded[strings.TrimSpace(label)]; ok {
			labels[idx] = "-"
		}
	}
}

func readExistingIELabels(libraryID, driveID string) []string {
	stateKey := storageutil.MediaStateKey(libraryID, driveID)
	path := filepath.Join(mediaStateDir(), sanitizeStateID(stateKey)+".ie")
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	labels := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" && trimmed != "-" {
			labels = append(labels, trimmed)
		}
	}
	return labels
}

func readExistingVaultLabels(libraryID, driveID string) []string {
	stateKey := storageutil.MediaStateKey(libraryID, driveID)
	path := filepath.Join(mediaStateDir(), sanitizeStateID(stateKey)+".vault")
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	labels := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" && trimmed != "-" {
			labels = append(labels, trimmed)
		}
	}
	return labels
}

func stableSlotLabels(slotCount, slotStart int, existing []string, cartridges []*domain.VirtualCartridge, active map[string]struct{}) []string {
	labels := make([]string, slotCount)
	placed := make(map[string]struct{})
	hasExistingSnapshot := len(existing) > 0
	for _, cartridge := range cartridges {
		if cartridge == nil || cartridge.AssignedSlotAddress == nil {
			continue
		}
		label := cartridgeSharedLabel(cartridge)
		if label == "" {
			continue
		}
		if _, ok := placed[label]; ok {
			continue
		}
		idx := *cartridge.AssignedSlotAddress - slotStart
		if idx < 0 || idx >= len(labels) {
			continue
		}
		if labels[idx] == "" || labels[idx] == "-" || labels[idx] == label {
			labels[idx] = label
			placed[label] = struct{}{}
		} else {
			log.Printf("slot assignment collision librarySlot=%d existing=%s cartridge=%s assignedSlot=%d", slotStart+idx, labels[idx], label, *cartridge.AssignedSlotAddress)
		}
	}
	for idx := 0; idx < slotCount && idx < len(existing); idx++ {
		if labels[idx] != "" {
			continue
		}
		label := strings.TrimSpace(existing[idx])
		if label == "" {
			labels[idx] = "-"
			continue
		}
		if _, ok := active[label]; ok {
			if _, alreadyPlaced := placed[label]; alreadyPlaced {
				labels[idx] = "-"
				continue
			}
			labels[idx] = label
			placed[label] = struct{}{}
		} else {
			labels[idx] = "-"
		}
	}
	for idx := 0; idx < slotCount; idx++ {
		if labels[idx] == "" {
			labels[idx] = "-"
		}
	}
	nextSlot := 0
	for _, cartridge := range cartridges {
		if cartridge == nil || cartridge.AssignedSlotAddress != nil || hasExistingSnapshot {
			continue
		}
		label := cartridgeSharedLabel(cartridge)
		if label == "" {
			continue
		}
		if _, ok := placed[label]; ok {
			continue
		}
		for nextSlot < len(labels) && labels[nextSlot] != "-" {
			nextSlot++
		}
		if nextSlot >= len(labels) {
			break
		}
		labels[nextSlot] = label
		placed[label] = struct{}{}
	}
	return labels
}

func exportedIELabels(portCount int, cartridges []*domain.VirtualCartridge) []string {
	if portCount <= 0 {
		portCount = 1
	}
	labels := make([]string, portCount)
	for idx := range labels {
		labels[idx] = "-"
	}
	for idx, cartridge := range cartridges {
		if idx >= len(labels) {
			break
		}
		label := cartridgeSharedLabel(cartridge)
		if label == "" {
			continue
		}
		labels[idx] = label
	}
	return labels
}

func exportedVaultLabels(cartridges []*domain.VirtualCartridge) []string {
	labels := make([]string, 0, len(cartridges))
	for _, cartridge := range cartridges {
		label := cartridgeSharedLabel(cartridge)
		if label == "" {
			continue
		}
		labels = append(labels, label)
	}
	return labels
}

func readDriveMediaState(libraryID, driveID string) (string, error) {
	return storageutil.ReadDriveMediaState(libraryID, driveID)
}

func writeDriveMediaState(libraryID, driveID, cartridgeID string) error {
	targetPath, err := storageutil.MediaStatePath(libraryID, driveID)
	if err != nil {
		return err
	}
	dir := filepath.Dir(targetPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmpPath := targetPath + ".tmp"
	payload := "cartridge=" + strings.TrimSpace(cartridgeID) + "\n"
	if err := os.WriteFile(tmpPath, []byte(payload), 0o644); err != nil {
		return err
	}
	return os.Rename(tmpPath, targetPath)
}

type cartridgeMetadata struct {
	CapacityBytes int64
	UsedBytes     int64
	PoolID        string
}

func cartridgeMetadataLabels(cartridge *domain.VirtualCartridge) []string {
	if cartridge == nil {
		return nil
	}
	seen := make(map[string]struct{})
	labels := make([]string, 0, 2)
	for _, raw := range []string{cartridge.CartridgeID, cartridge.Barcode} {
		label := strings.TrimSpace(raw)
		if label == "" {
			continue
		}
		key := strings.ToUpper(label)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		labels = append(labels, label)
	}
	return labels
}

func cartridgeMetadataPath(label string) string {
	return filepath.Join(mediaStateDir(), "cartridge_"+sanitizeStateID(label)+".meta")
}

func readCartridgeMetadata(cartridge *domain.VirtualCartridge) (*cartridgeMetadata, error) {
	for _, label := range cartridgeMetadataLabels(cartridge) {
		raw, err := os.ReadFile(cartridgeMetadataPath(label))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		meta := &cartridgeMetadata{UsedBytes: -1}
		for _, line := range strings.Split(string(raw), "\n") {
			key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
			if !ok {
				continue
			}
			key = strings.TrimSpace(key)
			value = strings.TrimSpace(value)
			if key == "pool_id" {
				meta.PoolID = value
				continue
			}
			parsed, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				continue
			}
			switch key {
			case "capacity_bytes":
				meta.CapacityBytes = parsed
			case "used_bytes":
				meta.UsedBytes = parsed
			}
		}
		return meta, nil
	}
	return nil, nil
}

func writeCartridgeMetadata(cartridge *domain.VirtualCartridge) error {
	if cartridge == nil {
		return domain.ErrInvalidInput
	}
	dir := mediaStateDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	capacityBytes := cartridge.CapacityBytes
	usedBytes := cartridge.UsedBytes
	if existing, err := readCartridgeMetadata(cartridge); err != nil {
		return err
	} else if existing != nil {
		if capacityBytes <= 0 && existing.CapacityBytes > 0 {
			capacityBytes = existing.CapacityBytes
		}
		if usedBytes == 0 && existing.UsedBytes > 0 {
			usedBytes = existing.UsedBytes
		}
	}
	payload := "cartridge_id=" + strings.TrimSpace(cartridge.CartridgeID) + "\n" +
		"pool_id=" + strings.TrimSpace(cartridge.PoolID) + "\n" +
		"capacity_bytes=" + strconv.FormatInt(capacityBytes, 10) + "\n" +
		"used_bytes=" + strconv.FormatInt(usedBytes, 10) + "\n"
	for _, label := range cartridgeMetadataLabels(cartridge) {
		if err := writeAtomicText(cartridgeMetadataPath(label), payload); err != nil {
			return err
		}
	}
	return nil
}

func removeCartridgeMetadataFile(cartridge *domain.VirtualCartridge) error {
	for _, label := range cartridgeMetadataLabels(cartridge) {
		path := cartridgeMetadataPath(label)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func writeAtomicText(targetPath, payload string) error {
	tmpPath := targetPath + ".tmp"
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(payload); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, targetPath); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return syncParentDir(targetPath)
}

func syncParentDir(path string) error {
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}

func mediaStateDir() string {
	return storageutil.MediaStateDir()
}

func sanitizeStateID(raw string) string {
	raw = strings.TrimSpace(strings.ToLower(raw))
	if raw == "" {
		return "unknown"
	}
	var b strings.Builder
	for _, ch := range raw {
		if (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '-' || ch == '_' {
			b.WriteRune(ch)
		} else {
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "" {
		return "unknown"
	}
	return out
}

func removeCartridgeLayoutArtifacts(cartridge *domain.VirtualCartridge) error {
	if cartridge == nil {
		return domain.ErrInvalidInput
	}

	targets, err := cartridgeLayoutArtifactDirs(cartridge)
	if err != nil {
		return err
	}
	approvedRoots := approvedCartridgeLayoutRoots(cartridge.PoolID)
	for target := range targets {
		if strings.TrimSpace(target) == "" {
			continue
		}
		if err := removeAllApprovedLayoutPath(target, approvedRoots); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func resetCartridgeLayoutArtifacts(cartridge *domain.VirtualCartridge) error {
	if cartridge == nil {
		return domain.ErrInvalidInput
	}
	targets, err := cartridgeLayoutArtifactDirs(cartridge)
	if err != nil {
		return err
	}
	for target := range targets {
		if strings.TrimSpace(target) == "" {
			continue
		}
		entries, err := os.ReadDir(target)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		for _, entry := range entries {
			if entry.IsDir() || !isShortEraseLayoutArtifact(entry.Name()) {
				continue
			}
			path := filepath.Join(target, entry.Name())
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return nil
}

func cartridgeLayoutArtifactDirs(cartridge *domain.VirtualCartridge) (map[string]struct{}, error) {
	return resolveCartridgeLayoutArtifactDirs(cartridge, approvedCartridgeLayoutRoots(cartridge.PoolID))
}

func resolveCartridgeLayoutArtifactDirs(cartridge *domain.VirtualCartridge, candidateRoots []string) (map[string]struct{}, error) {
	if cartridge == nil {
		return nil, domain.ErrInvalidInput
	}

	existingTargets := make(map[string]struct{})
	for _, root := range candidateRoots {
		root = strings.TrimSpace(root)
		if root == "" {
			continue
		}
		canonical := storageutil.CanonicalCartridgeLayoutDir(root, cartridge.LibraryID, cartridge.CartridgeID)
		exists, err := existingCartridgeLayoutDir(canonical)
		if err != nil {
			if errors.Is(err, storageutil.ErrStorageIdentityConflict) {
				return nil, domain.ErrIdentityConflict
			}
			return nil, err
		}
		if exists {
			existingTargets[canonical] = struct{}{}
		}
		legacyDirs, err := storageutil.LegacyCartridgeLayoutDirs(root, cartridge.CartridgeID)
		if err != nil {
			if errors.Is(err, storageutil.ErrStorageIdentityConflict) {
				return nil, domain.ErrIdentityConflict
			}
			return nil, err
		}
		for _, dir := range legacyDirs {
			existingTargets[dir] = struct{}{}
		}
	}
	if len(existingTargets) > 1 {
		return nil, domain.ErrAmbiguousLayout
	}
	if len(existingTargets) == 1 {
		return existingTargets, nil
	}
	return map[string]struct{}{
		storageutil.CanonicalCartridgeLayoutDir(storageutil.PoolStorageRoot(cartridge.PoolID), cartridge.LibraryID, cartridge.CartridgeID): {},
	}, nil
}

func existingCartridgeLayoutDir(path string) (bool, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return false, domain.ErrIdentityConflict
	}
	return info.IsDir(), nil
}

func approvedCartridgeLayoutRoots(poolID string) []string {
	candidateRoots := []string{
		storageutil.PoolStorageRoot(poolID),
		storageutil.ResolveStorageRoot(),
		"/var/lib/holo/storage",
		"/tmp/holo-storage",
	}
	if home, homeErr := os.UserHomeDir(); homeErr == nil {
		candidateRoots = append(candidateRoots, filepath.Join(home, ".local", "share", "holo", "storage"))
	}

	return candidateRoots
}

func removeAllApprovedLayoutPath(target string, approvedRoots []string) error {
	target = filepath.Clean(strings.TrimSpace(target))
	if target == "" || !filepath.IsAbs(target) {
		return domain.ErrInvalidInput
	}
	if _, err := os.Lstat(target); os.IsNotExist(err) {
		return nil
	}
	if evaluated, err := filepath.EvalSymlinks(target); err == nil {
		target = filepath.Clean(evaluated)
	}
	for _, root := range approvedRoots {
		root = strings.TrimSpace(root)
		if root == "" {
			continue
		}
		root = filepath.Clean(root)
		if evaluatedRoot, err := filepath.EvalSymlinks(root); err == nil {
			root = filepath.Clean(evaluatedRoot)
		}
		if target != root && pathWithinBase(root, target) {
			return os.RemoveAll(target)
		}
	}
	return domain.ErrInvalidInput
}

func isShortEraseLayoutArtifact(name string) bool {
	switch name {
	case "data.segment",
		"metadata.segment",
		"blk_map.segment",
		"lookup.segment",
		"reclaim.segment",
		"dedup.segment",
		"segment_index.segment",
		"filemarks.state",
		"usage.counters":
		return true
	}
	return strings.HasPrefix(name, "data_") && strings.HasSuffix(name, ".seg")
}
