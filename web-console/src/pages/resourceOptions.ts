import type { VirtualDrive, VirtualLibrary } from "../services/types";

export type LibraryTypeOption = {
  vendor: string;
  label: string;
  libraryType: string;
  supportedGenerations: string[];
  supportedEnterprise: string[];
  driveVendorPolicy: "open" | "mixed" | "vendor_locked";
  allowedDriveVendors?: string[];
  status?: "current" | "end_of_life";
};

export type DriveTypeOption = {
  vendor: string;
  label: string;
  driveType: string;
  generation: string;
  enterpriseProduct?: string;
};

export const LIBRARY_TYPE_OPTIONS: LibraryTypeOption[] = [
  { vendor: "IBM", label: "IBM TS3100", libraryType: "IBM TS3100", supportedGenerations: ["LTO3", "LTO4", "LTO5", "LTO6"], supportedEnterprise: [], driveVendorPolicy: "open", status: "end_of_life" },
  { vendor: "IBM", label: "IBM TS3200", libraryType: "IBM TS3200", supportedGenerations: ["LTO4", "LTO5", "LTO6"], supportedEnterprise: [], driveVendorPolicy: "open", status: "end_of_life" },
  { vendor: "IBM", label: "IBM TS3310", libraryType: "IBM TS3310", supportedGenerations: ["LTO5", "LTO6", "LTO7"], supportedEnterprise: [], driveVendorPolicy: "open", status: "end_of_life" },
  { vendor: "IBM", label: "IBM TS3500", libraryType: "IBM TS3500", supportedGenerations: ["LTO3", "LTO4", "LTO5", "LTO6", "LTO7", "LTO8", "LTO9"], supportedEnterprise: ["03592J1A", "03592E05", "03592E06"], driveVendorPolicy: "mixed", status: "end_of_life" },
  { vendor: "IBM", label: "IBM TS4300", libraryType: "IBM TS4300", supportedGenerations: ["LTO6", "LTO7", "LTO8", "LTO9", "LTO10"], supportedEnterprise: [], driveVendorPolicy: "open", status: "current" },
  { vendor: "IBM", label: "IBM TS4500", libraryType: "IBM TS4500", supportedGenerations: ["LTO7", "LTO8", "LTO9", "LTO10"], supportedEnterprise: ["03592E05", "03592E06"], driveVendorPolicy: "vendor_locked", status: "current" },
  { vendor: "IBM", label: "IBM Diamondback", libraryType: "IBM Diamondback", supportedGenerations: ["LTO8", "LTO9", "LTO10"], supportedEnterprise: ["03592E06"], driveVendorPolicy: "vendor_locked", status: "current" },

  { vendor: "HP/HPE", label: "HP ESL9000 Series", libraryType: "HP ESL9000 Series", supportedGenerations: ["LTO3", "LTO4"], supportedEnterprise: [], driveVendorPolicy: "open", status: "end_of_life" },
  { vendor: "HP/HPE", label: "HP ESL E-Series", libraryType: "HP ESL E-Series", supportedGenerations: ["LTO3", "LTO4", "LTO5"], supportedEnterprise: [], driveVendorPolicy: "open", status: "end_of_life" },
  { vendor: "HP/HPE", label: "HP EML E-Series", libraryType: "HP EML E-Series", supportedGenerations: ["LTO3", "LTO4", "LTO5"], supportedEnterprise: [], driveVendorPolicy: "open", status: "end_of_life" },
  { vendor: "HP/HPE", label: "HP MSL G3 Series", libraryType: "HP MSL G3 Series", supportedGenerations: ["LTO4", "LTO5", "LTO6"], supportedEnterprise: [], driveVendorPolicy: "open", status: "end_of_life" },
  { vendor: "HP/HPE", label: "HP/HPE MSL2024", libraryType: "HP/HPE MSL2024", supportedGenerations: ["LTO4", "LTO5", "LTO6", "LTO7"], supportedEnterprise: [], driveVendorPolicy: "open", status: "end_of_life" },
  { vendor: "HP/HPE", label: "HP/HPE MSL4048", libraryType: "HP/HPE MSL4048", supportedGenerations: ["LTO4", "LTO5", "LTO6", "LTO7"], supportedEnterprise: [], driveVendorPolicy: "open", status: "end_of_life" },
  { vendor: "HP/HPE", label: "HP/HPE MSL8096", libraryType: "HP/HPE MSL8096", supportedGenerations: ["LTO4", "LTO5", "LTO6", "LTO7"], supportedEnterprise: [], driveVendorPolicy: "open", status: "end_of_life" },
  { vendor: "HP/HPE", label: "HP MSL6000 Series", libraryType: "HP MSL6000 Series", supportedGenerations: ["LTO4", "LTO5", "LTO6"], supportedEnterprise: [], driveVendorPolicy: "open", status: "end_of_life" },
  { vendor: "HP/HPE", label: "HPE MSL3040", libraryType: "HPE MSL3040", supportedGenerations: ["LTO7", "LTO8", "LTO9"], supportedEnterprise: [], driveVendorPolicy: "open", status: "current" },
  { vendor: "HP/HPE", label: "HPE MSL6480", libraryType: "HPE MSL6480", supportedGenerations: ["LTO7", "LTO8", "LTO9"], supportedEnterprise: [], driveVendorPolicy: "open", status: "current" },

  { vendor: "Quantum", label: "ADIC Scalar 24", libraryType: "ADIC Scalar 24", supportedGenerations: ["LTO3", "LTO4", "LTO5"], supportedEnterprise: [], driveVendorPolicy: "open", status: "end_of_life" },
  { vendor: "Quantum", label: "ADIC Scalar 100", libraryType: "ADIC Scalar 100", supportedGenerations: ["LTO3", "LTO4", "LTO5"], supportedEnterprise: [], driveVendorPolicy: "open", status: "end_of_life" },
  { vendor: "Quantum", label: "Quantum Scalar i40", libraryType: "Quantum Scalar i40", supportedGenerations: ["LTO5", "LTO6", "LTO7"], supportedEnterprise: [], driveVendorPolicy: "open", status: "end_of_life" },
  { vendor: "Quantum", label: "Quantum Scalar i80", libraryType: "Quantum Scalar i80", supportedGenerations: ["LTO5", "LTO6", "LTO7"], supportedEnterprise: [], driveVendorPolicy: "open", status: "end_of_life" },
  { vendor: "Quantum", label: "Quantum Scalar i500", libraryType: "Quantum Scalar i500", supportedGenerations: ["LTO3", "LTO4", "LTO5", "LTO6", "LTO7"], supportedEnterprise: [], driveVendorPolicy: "open", status: "end_of_life" },
  { vendor: "Quantum", label: "Quantum Scalar i6000", libraryType: "Quantum Scalar i6000", supportedGenerations: ["LTO5", "LTO6", "LTO7", "LTO8", "LTO9"], supportedEnterprise: [], driveVendorPolicy: "open", status: "current" },
  { vendor: "Quantum", label: "Quantum Scalar i3", libraryType: "Quantum Scalar i3", supportedGenerations: ["LTO7", "LTO8", "LTO9"], supportedEnterprise: [], driveVendorPolicy: "open", status: "current" },
  { vendor: "Quantum", label: "Quantum Scalar i6", libraryType: "Quantum Scalar i6", supportedGenerations: ["LTO7", "LTO8", "LTO9"], supportedEnterprise: [], driveVendorPolicy: "open", status: "current" },
  { vendor: "Quantum", label: "Quantum SuperLoader 3", libraryType: "Quantum SuperLoader 3", supportedGenerations: ["LTO4", "LTO5", "LTO6", "LTO7", "LTO8", "LTO9"], supportedEnterprise: [], driveVendorPolicy: "open", status: "current" },

  { vendor: "Dell", label: "Dell PowerVault TL1000", libraryType: "Dell PowerVault TL1000", supportedGenerations: ["LTO5", "LTO6", "LTO7", "LTO8"], supportedEnterprise: [], driveVendorPolicy: "open", status: "current" },
  { vendor: "Dell", label: "Dell PowerVault TL2000", libraryType: "Dell PowerVault TL2000", supportedGenerations: ["LTO5", "LTO6", "LTO7", "LTO8", "LTO9"], supportedEnterprise: [], driveVendorPolicy: "open", status: "current" },
  { vendor: "Dell", label: "Dell PowerVault TL4000", libraryType: "Dell PowerVault TL4000", supportedGenerations: ["LTO5", "LTO6", "LTO7", "LTO8", "LTO9"], supportedEnterprise: [], driveVendorPolicy: "open", status: "current" },
  { vendor: "Dell", label: "Dell EMC ML3", libraryType: "Dell EMC ML3", supportedGenerations: ["LTO5", "LTO6", "LTO7", "LTO8", "LTO9"], supportedEnterprise: [], driveVendorPolicy: "open", status: "current" },
  { vendor: "Dell", label: "Dell PowerVault ML6000", libraryType: "Dell PowerVault ML6000", supportedGenerations: ["LTO4", "LTO5", "LTO6", "LTO7", "LTO8", "LTO9"], supportedEnterprise: [], driveVendorPolicy: "open", status: "end_of_life" },

  { vendor: "Oracle / StorageTek", label: "StorageTek L20", libraryType: "StorageTek L20", supportedGenerations: ["LTO3"], supportedEnterprise: [], driveVendorPolicy: "open", status: "end_of_life" },
  { vendor: "Oracle / StorageTek", label: "StorageTek L80", libraryType: "StorageTek L80", supportedGenerations: ["LTO3", "LTO4", "LTO5"], supportedEnterprise: [], driveVendorPolicy: "open", status: "end_of_life" },
  { vendor: "Oracle / StorageTek", label: "StorageTek L700", libraryType: "StorageTek L700", supportedGenerations: [], supportedEnterprise: ["T9840A", "T9840B", "T9840C", "T9840D", "T9940A", "T9940B"], driveVendorPolicy: "vendor_locked", status: "end_of_life" },
  { vendor: "Oracle / StorageTek", label: "StorageTek SL150", libraryType: "StorageTek SL150", supportedGenerations: ["LTO5", "LTO6", "LTO7", "LTO8", "LTO9"], supportedEnterprise: [], driveVendorPolicy: "open", allowedDriveVendors: ["HP/HPE", "IBM"], status: "current" },
  { vendor: "Oracle / StorageTek", label: "StorageTek SL3000", libraryType: "StorageTek SL3000", supportedGenerations: ["LTO5", "LTO6", "LTO7", "LTO8"], supportedEnterprise: ["T9840D", "T10000A", "T10000B", "T10000C", "T10000D"], driveVendorPolicy: "mixed", status: "current" },
  { vendor: "Oracle / StorageTek", label: "StorageTek SL4000", libraryType: "StorageTek SL4000", supportedGenerations: ["LTO5", "LTO6", "LTO7", "LTO8", "LTO9"], supportedEnterprise: ["T10000A", "T10000B", "T10000C", "T10000D"], driveVendorPolicy: "mixed", status: "current" },
  { vendor: "Oracle / StorageTek", label: "StorageTek SL8500", libraryType: "StorageTek SL8500", supportedGenerations: ["LTO3", "LTO4", "LTO5", "LTO6"], supportedEnterprise: ["T9840D", "T10000A", "T10000B", "T10000C", "T10000D"], driveVendorPolicy: "mixed", status: "current" },

  { vendor: "Spectra Logic", label: "Spectra T50e", libraryType: "Spectra T50e", supportedGenerations: ["LTO4", "LTO5", "LTO6", "LTO7"], supportedEnterprise: [], driveVendorPolicy: "open", status: "end_of_life" },
  { vendor: "Spectra Logic", label: "Spectra T120", libraryType: "Spectra T120", supportedGenerations: ["LTO5", "LTO6", "LTO7", "LTO8"], supportedEnterprise: [], driveVendorPolicy: "open", status: "end_of_life" },
  { vendor: "Spectra Logic", label: "Spectra T200", libraryType: "Spectra T200", supportedGenerations: ["LTO5", "LTO6", "LTO7", "LTO8", "LTO9"], supportedEnterprise: [], driveVendorPolicy: "open", status: "current" },
  { vendor: "Spectra Logic", label: "Spectra T380", libraryType: "Spectra T380", supportedGenerations: ["LTO6", "LTO7", "LTO8", "LTO9"], supportedEnterprise: [], driveVendorPolicy: "open", status: "current" },
  { vendor: "Spectra Logic", label: "Spectra T680", libraryType: "Spectra T680", supportedGenerations: ["LTO6", "LTO7", "LTO8", "LTO9"], supportedEnterprise: [], driveVendorPolicy: "open", status: "current" },
  { vendor: "Spectra Logic", label: "Spectra T950", libraryType: "Spectra T950", supportedGenerations: ["LTO5", "LTO6", "LTO7", "LTO8", "LTO9"], supportedEnterprise: ["T10000A", "T10000B", "T10000C", "T10000D"], driveVendorPolicy: "open", status: "current" },
  { vendor: "Spectra Logic", label: "Spectra T950v", libraryType: "Spectra T950v", supportedGenerations: ["LTO7", "LTO8", "LTO9"], supportedEnterprise: ["T10000C", "T10000D"], driveVendorPolicy: "open", status: "current" },
  { vendor: "Spectra Logic", label: "Spectra TFinity", libraryType: "Spectra TFinity", supportedGenerations: ["LTO6", "LTO7", "LTO8", "LTO9", "LTO10"], supportedEnterprise: ["T10000B", "T10000C", "T10000D"], driveVendorPolicy: "open", status: "current" },
  { vendor: "Spectra Logic", label: "Spectra TFinity ExaScale", libraryType: "Spectra TFinity ExaScale", supportedGenerations: ["LTO8", "LTO9", "LTO10"], supportedEnterprise: ["T10000D"], driveVendorPolicy: "open", status: "current" },
  { vendor: "Spectra Logic", label: "Spectra Stack", libraryType: "Spectra Stack", supportedGenerations: ["LTO7", "LTO8", "LTO9"], supportedEnterprise: [], driveVendorPolicy: "open", status: "current" },
  { vendor: "Spectra Logic", label: "Spectra Python", libraryType: "Spectra Python", supportedGenerations: ["LTO3", "LTO4", "LTO5", "LTO6"], supportedEnterprise: [], driveVendorPolicy: "open", status: "end_of_life" },

  { vendor: "Overland / Tandberg", label: "Overland NEO Series", libraryType: "Overland NEO Series", supportedGenerations: ["LTO3", "LTO4", "LTO5"], supportedEnterprise: [], driveVendorPolicy: "open", status: "end_of_life" },
  { vendor: "Overland / Tandberg", label: "Overland FlexStor II", libraryType: "Overland FlexStor II", supportedGenerations: ["LTO4", "LTO5", "LTO6"], supportedEnterprise: [], driveVendorPolicy: "open", status: "end_of_life" },
  { vendor: "Overland / Tandberg", label: "Overland NEOxl / MultiStak", libraryType: "Overland NEOxl / MultiStak", supportedGenerations: ["LTO5", "LTO6", "LTO7"], supportedEnterprise: [], driveVendorPolicy: "open", status: "end_of_life" },
  { vendor: "Overland / Tandberg", label: "Tandberg NEOs T24", libraryType: "Tandberg NEOs T24", supportedGenerations: ["LTO5", "LTO6", "LTO7"], supportedEnterprise: [], driveVendorPolicy: "open", status: "end_of_life" },
  { vendor: "Overland / Tandberg", label: "Tandberg NEOxl 40", libraryType: "Tandberg NEOxl 40", supportedGenerations: ["LTO5", "LTO6", "LTO7", "LTO8"], supportedEnterprise: [], driveVendorPolicy: "open", status: "end_of_life" },
  { vendor: "Overland / Tandberg", label: "Tandberg NEOxl 80", libraryType: "Tandberg NEOxl 80", supportedGenerations: ["LTO5", "LTO6", "LTO7", "LTO8"], supportedEnterprise: [], driveVendorPolicy: "open", status: "end_of_life" },
];

export const DRIVE_TYPE_OPTIONS: DriveTypeOption[] = [
  { vendor: "IBM", label: "IBM TS2230 (LTO-3 / ULT3580-TD3)", driveType: "IBM TS2230", generation: "LTO3" },
  { vendor: "IBM", label: "IBM TS2240 (LTO-4 / ULT3580-TD4)", driveType: "IBM TS2240", generation: "LTO4" },
  { vendor: "IBM", label: "IBM TS2250 (LTO-5 / ULT3580-TD5)", driveType: "IBM TS2250", generation: "LTO5" },
  { vendor: "IBM", label: "IBM TS2260 (LTO-6 / ULT3580-TD6)", driveType: "IBM TS2260", generation: "LTO6" },
  { vendor: "IBM", label: "IBM TS2270 (LTO-7 / ULT3580-TD7)", driveType: "IBM TS2270", generation: "LTO7" },
  { vendor: "IBM", label: "IBM TS2280 (LTO-8 / ULT3580-TD8)", driveType: "IBM TS2280", generation: "LTO8" },
  { vendor: "IBM", label: "IBM TS2290 (LTO-9 / ULT3580-TD9)", driveType: "IBM TS2290", generation: "LTO9" },
  { vendor: "IBM", label: "IBM LTO-10 Tape Drive (ULT3580-TDA)", driveType: "IBM LTO-10 Tape Drive", generation: "LTO10" },
  { vendor: "HP/HPE", label: "HP Ultrium 920 (LTO-3)", driveType: "HP Ultrium 920", generation: "LTO3" },
  { vendor: "HP/HPE", label: "HP Ultrium 960 (LTO-3)", driveType: "HP Ultrium 960", generation: "LTO3" },
  { vendor: "HP/HPE", label: "HP Ultrium 1760 (LTO-4)", driveType: "HP Ultrium 1760", generation: "LTO4" },
  { vendor: "HP/HPE", label: "HP Ultrium 1840 (LTO-4)", driveType: "HP Ultrium 1840", generation: "LTO4" },
  { vendor: "HP/HPE", label: "HP Ultrium 3000 (LTO-5)", driveType: "HP Ultrium 3000", generation: "LTO5" },
  { vendor: "HP/HPE", label: "HP Ultrium 3280 (LTO-5)", driveType: "HP Ultrium 3280", generation: "LTO5" },
  { vendor: "HP/HPE", label: "HP StoreEver Ultrium 6250 (LTO-6)", driveType: "HP StoreEver Ultrium 6250", generation: "LTO6" },
  { vendor: "HP/HPE", label: "HP StoreEver Ultrium 6650 (LTO-6)", driveType: "HP StoreEver Ultrium 6650", generation: "LTO6" },
  { vendor: "HP/HPE", label: "HPE StoreEver LTO-7 Ultrium 15000", driveType: "HPE StoreEver LTO-7 Ultrium 15000", generation: "LTO7" },
  { vendor: "HP/HPE", label: "HPE StoreEver LTO-8 Ultrium 30750", driveType: "HPE StoreEver LTO-8 Ultrium 30750", generation: "LTO8" },
  { vendor: "HP/HPE", label: "HPE StoreEver LTO-9 Ultrium 45000", driveType: "HPE StoreEver LTO-9 Ultrium 45000", generation: "LTO9" },
  { vendor: "Quantum", label: "Quantum LTO-3 Tape Drive", driveType: "Quantum LTO-3 Tape Drive", generation: "LTO3" },
  { vendor: "Quantum", label: "Quantum LTO-4 Tape Drive", driveType: "Quantum LTO-4 Tape Drive", generation: "LTO4" },
  { vendor: "Quantum", label: "Quantum LTO-5 Tape Drive", driveType: "Quantum LTO-5 Tape Drive", generation: "LTO5" },
  { vendor: "Quantum", label: "Quantum LTO-6 Tape Drive", driveType: "Quantum LTO-6 Tape Drive", generation: "LTO6" },
  { vendor: "Quantum", label: "Quantum LTO-7 Tape Drive", driveType: "Quantum LTO-7 Tape Drive", generation: "LTO7" },
  { vendor: "Quantum", label: "Quantum LTO-8 Tape Drive", driveType: "Quantum LTO-8 Tape Drive", generation: "LTO8" },
  { vendor: "Quantum", label: "Quantum LTO-9 Tape Drive", driveType: "Quantum LTO-9 Tape Drive", generation: "LTO9" },
  { vendor: "Quantum", label: "Quantum LTO-10 Tape Drive", driveType: "Quantum LTO-10 Tape Drive", generation: "LTO10" },
  { vendor: "Oracle / StorageTek", label: "StorageTek T9840A", driveType: "StorageTek T9840A", generation: "proprietary", enterpriseProduct: "T9840A" },
  { vendor: "Oracle / StorageTek", label: "StorageTek T9840B", driveType: "StorageTek T9840B", generation: "proprietary", enterpriseProduct: "T9840B" },
  { vendor: "Oracle / StorageTek", label: "StorageTek T9840C", driveType: "StorageTek T9840C", generation: "proprietary", enterpriseProduct: "T9840C" },
  { vendor: "Oracle / StorageTek", label: "StorageTek T9840D", driveType: "StorageTek T9840D", generation: "proprietary", enterpriseProduct: "T9840D" },
  { vendor: "Oracle / StorageTek", label: "StorageTek T9940A", driveType: "StorageTek T9940A", generation: "proprietary", enterpriseProduct: "T9940A" },
  { vendor: "Oracle / StorageTek", label: "StorageTek T9940B", driveType: "StorageTek T9940B", generation: "proprietary", enterpriseProduct: "T9940B" },
  { vendor: "Oracle / StorageTek", label: "StorageTek T10000A", driveType: "StorageTek T10000A", generation: "proprietary", enterpriseProduct: "T10000A" },
  { vendor: "Oracle / StorageTek", label: "StorageTek T10000B", driveType: "StorageTek T10000B", generation: "proprietary", enterpriseProduct: "T10000B" },
  { vendor: "Oracle / StorageTek", label: "StorageTek T10000C", driveType: "StorageTek T10000C", generation: "proprietary", enterpriseProduct: "T10000C" },
  { vendor: "Oracle / StorageTek", label: "StorageTek T10000D", driveType: "StorageTek T10000D", generation: "proprietary", enterpriseProduct: "T10000D" },
  { vendor: "IBM", label: "IBM 3592 J1A", driveType: "IBM 3592 J1A", generation: "proprietary", enterpriseProduct: "03592J1A" },
  { vendor: "IBM", label: "IBM 3592 E05", driveType: "IBM 3592 E05", generation: "proprietary", enterpriseProduct: "03592E05" },
  { vendor: "IBM", label: "IBM 3592 E06", driveType: "IBM 3592 E06", generation: "proprietary", enterpriseProduct: "03592E06" },
];

export const DEFAULT_LIBRARY_OPTION =
  LIBRARY_TYPE_OPTIONS.find((item) => item.vendor === "IBM" && item.libraryType === "IBM TS3500") ||
  LIBRARY_TYPE_OPTIONS[0];

export const DEFAULT_DRIVE_OPTION =
  DRIVE_TYPE_OPTIONS.find((item) => item.vendor === "IBM" && item.driveType === "IBM TS2260") ||
  DRIVE_TYPE_OPTIONS[0];

export const MAX_LIBRARY_DRIVES = 4;

const GIB = 1024 ** 3;
const TIB = 1024 ** 4;

const LTO_CAPACITY_BYTES_BY_GENERATION: Record<number, number> = {
  1: 100 * GIB,
  2: 200 * GIB,
  3: 400 * GIB,
  4: 800 * GIB,
  5: Math.floor(1.5 * TIB),
  6: Math.floor(2.5 * TIB),
  7: 6 * TIB,
  8: 12 * TIB,
  9: 18 * TIB,
  10: 40 * TIB,
};

const DRIVE_TYPE_LTO_GENERATION_MAP = Object.fromEntries(
  DRIVE_TYPE_OPTIONS.filter((item) => item.generation.startsWith("LTO")).map((item) => [
    item.driveType.toUpperCase(),
    Number.parseInt(item.generation.replace("LTO", ""), 10),
  ])
) as Record<string, number>;

function clampLTOGeneration(input: number): number {
  if (!Number.isFinite(input)) {
    return 6;
  }
  const value = Math.floor(input);
  if (value < 1) {
    return 1;
  }
  if (value > 99) {
    return 99;
  }
  return value;
}

function parseLTOGenerationFromDriveType(rawDriveType: string): number | null {
  const driveType = rawDriveType.trim().toUpperCase();
  if (!driveType) {
    return null;
  }

  if (DRIVE_TYPE_LTO_GENERATION_MAP[driveType]) {
    return DRIVE_TYPE_LTO_GENERATION_MAP[driveType];
  }

  const tdMatch = /ULT3580[-_]?TD(\d{1,2})/.exec(driveType);
  if (tdMatch) {
    return clampLTOGeneration(Number.parseInt(tdMatch[1], 10));
  }

  const ltoMatch = /LTO[-_]?(\d{1,2})/.exec(driveType);
  if (ltoMatch) {
    return clampLTOGeneration(Number.parseInt(ltoMatch[1], 10));
  }

  return null;
}

export type DriveTapeProfile = {
  ltoGeneration: number;
  capacityBytes: number;
};

export function resolveTapeProfileFromDriveType(driveType?: string): DriveTapeProfile {
  const parsed = parseLTOGenerationFromDriveType(driveType || "");
  const ltoGeneration = parsed ?? 6;
  const capacityBytes =
    LTO_CAPACITY_BYTES_BY_GENERATION[ltoGeneration] || LTO_CAPACITY_BYTES_BY_GENERATION[6];
  return { ltoGeneration, capacityBytes };
}

export function libraryTypeOptionsForVendor(vendor: string): LibraryTypeOption[] {
  return LIBRARY_TYPE_OPTIONS.filter((item) => item.vendor === vendor);
}

export function availableVendors(): string[] {
  return [...new Set(LIBRARY_TYPE_OPTIONS.map((item) => item.vendor))];
}

export function libraryOptionByType(libraryType: string): LibraryTypeOption | undefined {
  return LIBRARY_TYPE_OPTIONS.find((item) => item.libraryType === libraryType);
}

export function driveTypeOptionsForLibrary(libraryType: string): DriveTypeOption[] {
  const library = libraryOptionByType(libraryType);
  if (!library) {
    return DRIVE_TYPE_OPTIONS;
  }

  const lto = new Set(library.supportedGenerations);
  const enterprise = new Set(library.supportedEnterprise);
  const allowedDriveVendors = new Set(library.allowedDriveVendors || []);
  return DRIVE_TYPE_OPTIONS.filter((drive) => {
    if (allowedDriveVendors.size > 0 && !allowedDriveVendors.has(drive.vendor)) {
      return false;
    }
    const compatible =
      (drive.generation.startsWith("LTO") && lto.has(drive.generation)) ||
      (!!drive.enterpriseProduct && enterprise.has(drive.enterpriseProduct));
    if (!compatible) {
      return false;
    }
    if (library.driveVendorPolicy === "vendor_locked") {
      if (library.vendor === "Oracle / StorageTek") {
        return drive.vendor === "Oracle / StorageTek";
      }
      return drive.vendor === library.vendor;
    }
    return true;
  });
}

function slugifyValue(input: string): string {
  const cleaned = input
    .trim()
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/(^-|-$)/g, "");
  return cleaned || "holo";
}

export function nextLibraryId(name: string, existing: VirtualLibrary[]): string {
  const used = new Set(existing.map((item) => item.libraryId));
  const base = slugifyValue(name);
  if (!used.has(base)) {
    return base;
  }
  let idx = 1;
  while (used.has(`${base}-${idx}`)) {
    idx += 1;
  }
  return `${base}-${idx}`;
}

export function nextDriveSuffix(drives: VirtualDrive[], libraryId: string): number {
  let max = 0;
  for (const drive of drives) {
    if (drive.libraryId !== libraryId) {
      continue;
    }
    const parts = drive.driveId.split("-");
    const tail = Number.parseInt(parts[parts.length - 1] || "", 10);
    if (Number.isFinite(tail) && tail > max) {
      max = tail;
    }
  }
  return max + 1;
}
