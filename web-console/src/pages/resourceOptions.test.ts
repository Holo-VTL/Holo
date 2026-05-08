import { describe, expect, it } from "vitest";
import {
  availableVendors,
  driveTypeOptionsForLibrary,
  libraryTypeOptionsForVendor,
  resolveTapeProfileFromDriveType,
} from "./resourceOptions";

describe("resourceOptions", () => {
  it("exposes vendor to library options from the emulation master list", () => {
    expect(availableVendors()).toContain("IBM");
    expect(availableVendors()).toContain("HP/HPE");
    expect(availableVendors()).toContain("Quantum");
    expect(availableVendors()).toContain("Oracle / StorageTek");
    expect(libraryTypeOptionsForVendor("IBM").map((item) => item.libraryType)).toContain("IBM TS3500");
    expect(libraryTypeOptionsForVendor("HP/HPE").map((item) => item.libraryType)).toContain("HPE MSL3040");
  });

  it("filters drives by the selected library compatibility rules", () => {
    const ts4300 = driveTypeOptionsForLibrary("IBM TS4300").map((item) => item.driveType);
    expect(ts4300).toContain("IBM TS2290");
    expect(ts4300).toContain("IBM LTO-10 Tape Drive");
    expect(ts4300).not.toContain("IBM TS2250");

    const sl150 = driveTypeOptionsForLibrary("StorageTek SL150").map((item) => item.driveType);
    expect(sl150).toContain("IBM TS2290");
    expect(sl150).toContain("HPE StoreEver LTO-9 Ultrium 45000");
    expect(sl150).not.toContain("Quantum LTO-9 Tape Drive");
    expect(sl150).not.toContain("StorageTek T10000D");

    const sl8500 = driveTypeOptionsForLibrary("StorageTek SL8500").map((item) => item.driveType);
    expect(sl8500).toContain("IBM TS2260");
    expect(sl8500).toContain("StorageTek T10000D");
  });

  it("resolves cartridge defaults from marketed drive names", () => {
    expect(resolveTapeProfileFromDriveType("IBM TS2290")).toMatchObject({ ltoGeneration: 9 });
    expect(resolveTapeProfileFromDriveType("Quantum LTO-10 Tape Drive")).toMatchObject({ ltoGeneration: 10 });
  });
});
