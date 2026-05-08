import { describe, expect, it } from "vitest";
import { barcodeIndex, makeBarcode, nextBarcode } from "./barcode";

describe("barcode helper", () => {
  it("creates barcode format VTA000L06", () => {
    expect(makeBarcode(0, 6)).toBe("VTA000L06");
    expect(makeBarcode(1000, 6)).toBe("VTB000L06");
  });

  it("parses valid barcode index", () => {
    expect(barcodeIndex("VTA000L06")).toBe(0);
    expect(barcodeIndex("VTB005L06")).toBe(1005);
    expect(barcodeIndex("invalid")).toBeNull();
  });

  it("finds next barcode from existing collection", () => {
    const existing = ["VTA000L06", "VTA001L06", "VTA003L06"];
    expect(nextBarcode(existing, 6)).toBe("VTA004L06");
  });
});
