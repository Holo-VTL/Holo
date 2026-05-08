const BARCODE_RE = /^VT([A-Z])(\d{3})L(\d{2})$/;

function clampGeneration(input: number): number {
  if (!Number.isFinite(input) || input < 1) {
    return 6;
  }
  if (input > 99) {
    return 99;
  }
  return Math.floor(input);
}

export function barcodeIndex(barcode: string): number | null {
  const m = BARCODE_RE.exec(barcode.trim());
  if (!m) {
    return null;
  }
  const letter = m[1].charCodeAt(0) - 65;
  const num = Number.parseInt(m[2], 10);
  return letter * 1000 + num;
}

export function makeBarcode(index: number, ltoGeneration: number): string {
  const safeIndex = Math.max(0, Math.min(index, 25999));
  const letterCode = Math.floor(safeIndex / 1000);
  const numberPart = safeIndex % 1000;
  const letter = String.fromCharCode(65 + letterCode);
  const lto = String(clampGeneration(ltoGeneration)).padStart(2, "0");
  return `VT${letter}${String(numberPart).padStart(3, "0")}L${lto}`;
}

export function nextBarcode(existing: string[], ltoGeneration: number): string {
  let maxIndex = -1;
  for (const value of existing) {
    const idx = barcodeIndex(value);
    if (idx !== null && idx > maxIndex) {
      maxIndex = idx;
    }
  }
  return makeBarcode(maxIndex + 1, ltoGeneration);
}
