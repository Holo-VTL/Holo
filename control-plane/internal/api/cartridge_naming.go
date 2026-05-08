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

const maxTapeSequence = 26 * 1000

func (h *ResourcesHandler) resolveCartridgeIdentity(ctx context.Context, req createCartridgeRequest) (string, string, error) {
	libraryID := strings.TrimSpace(req.LibraryID)
	cartridgeID := strings.TrimSpace(req.CartridgeID)
	barcode := strings.TrimSpace(req.Barcode)
	generation, err := resolveRequestedLTOGeneration(req.LTOGeneration, req.MediaType)
	if err != nil {
		return "", "", domain.ErrInvalidInput
	}

	if cartridgeID == "" && barcode == "" {
		if generation == 0 {
			return "", "", domain.ErrInvalidInput
		}
		label, labelErr := h.nextTapeLabel(ctx, libraryID, generation)
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

func (h *ResourcesHandler) nextTapeLabel(ctx context.Context, libraryID string, generation int) (string, error) {
	usedSequences := make(map[int]struct{})
	usedLabels := make(map[string]struct{})
	for _, cartridge := range h.repo.ListCartridges(ctx) {
		if cartridge == nil || cartridge.LibraryID != libraryID {
			continue
		}
		markUsedTapeLabel(usedSequences, usedLabels, cartridge.CartridgeID)
		markUsedTapeLabel(usedSequences, usedLabels, cartridge.Barcode)
	}
	for sequence := 0; sequence < maxTapeSequence; sequence++ {
		if _, exists := usedSequences[sequence]; exists {
			continue
		}
		label := formatTapeLabel(sequence, generation)
		if _, exists := usedLabels[strings.ToUpper(label)]; exists {
			continue
		}
		return label, nil
	}
	return "", domain.ErrConflict
}

func markUsedTapeLabel(usedSequences map[int]struct{}, usedLabels map[string]struct{}, raw string) {
	normalized, sequence, _, ok := normalizeTapeLabel(raw)
	if !ok {
		return
	}
	if sequence >= 0 {
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

func formatTapeLabel(sequence, generation int) string {
	letter := rune('A' + (sequence / 1000))
	index := sequence % 1000
	return fmt.Sprintf("VT%c%03dL%02d", letter, index, generation)
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
