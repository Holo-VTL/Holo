package api

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
)

var tapeLabelPattern = regexp.MustCompile(`^([A-Z0-9]{1,3})([0-9]{3})L([0-9]{2})$`)

func (h *ResourcesHandler) resolveCartridgeIdentity(ctx context.Context, req createCartridgeRequest) (string, string, error) {
	libraryID := strings.TrimSpace(req.LibraryID)
	cartridgeID := strings.TrimSpace(req.CartridgeID)
	barcode := strings.TrimSpace(req.Barcode)
	barcodePrefix := strings.TrimSpace(req.BarcodePrefix)
	generation, err := resolveRequestedLTOGeneration(req.LTOGeneration, req.MediaType)
	if err != nil {
		return "", "", domain.ErrInvalidInput
	}

	if cartridgeID == "" && barcode == "" {
		if generation == 0 {
			return "", "", domain.ErrInvalidInput
		}
		label, labelErr := h.nextTapeLabel(ctx, libraryID, barcodePrefix, generation)
		if labelErr != nil {
			return "", "", labelErr
		}
		return label, label, nil
	}

	if cartridgeID == "" {
		cartridgeID = barcode
	}
	if barcode == "" {
		barcode = cartridgeID
	}

	normalizedID, _, idGeneration, idOK := normalizeTapeLabel(cartridgeID)
	normalizedBarcode, _, barcodeGeneration, barcodeOK := normalizeTapeLabel(barcode)
	if !idOK || !barcodeOK || normalizedID != normalizedBarcode || idGeneration != barcodeGeneration {
		return "", "", domain.ErrInvalidInput
	}
	if generation > 0 && idGeneration != generation {
		return "", "", domain.ErrInvalidInput
	}

	return normalizedID, normalizedBarcode, nil
}

func (h *ResourcesHandler) nextTapeLabel(ctx context.Context, libraryID, prefix string, generation int) (string, error) {
	prefix, err := normalizeTapePrefix(prefix)
	if err != nil {
		return "", domain.ErrInvalidInput
	}
	usedSequences := make(map[int]struct{})
	usedLabels := make(map[string]struct{})
	for _, cartridge := range h.repo.ListCartridges(ctx) {
		if cartridge == nil || cartridge.LibraryID != libraryID {
			continue
		}
		markUsedTapeLabel(usedSequences, usedLabels, cartridge.CartridgeID, prefix, generation)
		markUsedTapeLabel(usedSequences, usedLabels, cartridge.Barcode, prefix, generation)
	}
	for _, label := range h.repo.ListRetiredCartridgeBarcodes(ctx) {
		markUsedTapeLabel(usedSequences, usedLabels, label, prefix, generation)
	}
	for sequence := 0; sequence < 1000; sequence++ {
		if _, exists := usedSequences[sequence]; exists {
			continue
		}
		label := formatTapeLabel(prefix, sequence, generation)
		if _, exists := usedLabels[strings.ToUpper(label)]; exists {
			continue
		}
		return label, nil
	}
	return "", domain.ErrConflict
}

func markUsedTapeLabel(usedSequences map[int]struct{}, usedLabels map[string]struct{}, raw, prefix string, generation int) {
	normalized, sequence, _, ok := normalizeTapeLabel(raw)
	if !ok {
		return
	}
	if labelPrefix, labelSequence, labelGeneration, ok := parseTapeLabelParts(normalized); ok && labelPrefix == prefix && labelGeneration == generation {
		usedSequences[labelSequence] = struct{}{}
	}
	// Legacy auto-labeling folded VTA/VTB/... into one VTA sequence space,
	// so VTA generation still reserves labels parsed by the old normalizer.
	if sequence >= 0 && prefix == "VTA" {
		usedSequences[sequence] = struct{}{}
	}
	usedLabels[normalized] = struct{}{}
}

func resolveRequestedLTOGeneration(generation int, mediaType string) (int, error) {
	mediaType = strings.TrimSpace(mediaType)
	if generation == 0 && mediaType == "" {
		return 0, nil
	}
	if generation != 0 && (generation < 1 || generation > 99) {
		return 0, fmt.Errorf("invalid lto generation")
	}
	if mediaType == "" {
		return generation, nil
	}
	parsed, err := parseLTOGeneration(mediaType)
	if err != nil {
		return 0, err
	}
	if generation != 0 && generation != parsed {
		return 0, fmt.Errorf("generation mismatch")
	}
	return parsed, nil
}

func parseLTOGeneration(raw string) (int, error) {
	normalized := strings.ToUpper(strings.TrimSpace(raw))
	normalized = strings.ReplaceAll(normalized, " ", "")
	normalized = strings.ReplaceAll(normalized, "-", "")
	normalized = strings.ReplaceAll(normalized, "_", "")

	switch {
	case strings.HasPrefix(normalized, "LTO"):
		normalized = strings.TrimPrefix(normalized, "LTO")
	case strings.HasPrefix(normalized, "LT"):
		normalized = strings.TrimPrefix(normalized, "LT")
	case strings.HasPrefix(normalized, "L"):
		normalized = strings.TrimPrefix(normalized, "L")
	}
	if normalized == "" {
		return 0, fmt.Errorf("invalid media type")
	}
	value, err := strconv.Atoi(normalized)
	if err != nil || value < 1 || value > 99 {
		return 0, fmt.Errorf("invalid media type")
	}
	return value, nil
}

func normalizeTapePrefix(raw string) (string, error) {
	prefix := strings.ToUpper(strings.TrimSpace(raw))
	if prefix == "" {
		return "VTA", nil
	}
	if len(prefix) > 3 {
		return "", fmt.Errorf("invalid tape prefix")
	}
	for _, ch := range prefix {
		if (ch < 'A' || ch > 'Z') && (ch < '0' || ch > '9') {
			return "", fmt.Errorf("invalid tape prefix")
		}
	}
	return prefix, nil
}

func formatTapeLabel(prefix string, sequence, generation int) string {
	return fmt.Sprintf("%s%03dL%02d", prefix, sequence, generation)
}

func normalizeTapeLabel(raw string) (string, int, int, bool) {
	candidate := strings.ToUpper(strings.TrimSpace(raw))
	match := tapeLabelPattern.FindStringSubmatch(candidate)
	if len(match) != 4 {
		return "", 0, 0, false
	}
	number, err := strconv.Atoi(match[2])
	if err != nil {
		return "", 0, 0, false
	}
	generation, err := strconv.Atoi(match[3])
	if err != nil {
		return "", 0, 0, false
	}
	prefix := match[1]
	sequence := -1
	if len(prefix) == 3 && strings.HasPrefix(prefix, "VT") {
		suffix := prefix[2]
		if suffix >= 'A' && suffix <= 'Z' {
			sequence = int(suffix-'A')*1000 + number
		}
	}
	return candidate, sequence, generation, true
}

func parseTapeLabelParts(raw string) (string, int, int, bool) {
	candidate := strings.ToUpper(strings.TrimSpace(raw))
	match := tapeLabelPattern.FindStringSubmatch(candidate)
	if len(match) != 4 {
		return "", 0, 0, false
	}
	sequence, err := strconv.Atoi(match[2])
	if err != nil {
		return "", 0, 0, false
	}
	generation, err := strconv.Atoi(match[3])
	if err != nil {
		return "", 0, 0, false
	}
	return match[1], sequence, generation, true
}
