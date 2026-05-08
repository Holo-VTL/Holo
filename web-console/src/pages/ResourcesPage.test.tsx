import { screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { renderWithProviders } from "../test/renderWithProviders";
import { ResourcesPage } from "./ResourcesPage";

vi.mock("../services/api", () => ({
  api: {
    resources: {
      listLibraries: vi.fn().mockResolvedValue([{ libraryId: "lib-a", name: "Lib A", iqn: "iqn.a" }]),
      listDrives: vi.fn().mockResolvedValue([{ driveId: "drive-a", libraryId: "lib-a", slot: 1 }]),
      listCartridges: vi.fn().mockResolvedValue([{ cartridgeId: "car-a", poolId: "pool-a", libraryId: "lib-a", barcode: "VTA000L06", capacityBytes: 1000 }]),
      createLibrary: vi.fn(),
      createDrive: vi.fn(),
      createCartridge: vi.fn(),
      deleteLibrary: vi.fn(),
      deleteDrive: vi.fn(),
      deleteCartridge: vi.fn(),
      eraseCartridge: vi.fn(),
    },
    storage: {
      listPools: vi.fn().mockResolvedValue([
        { poolId: "pool-a", name: "Pool A", status: "active", disks: [{ devicePath: "/dev/sdb", sizeBytes: 1000, attachedAt: new Date().toISOString() }], capacity: { totalBytes: 1000, usedBytes: 0, freeBytes: 1000, usedPercent: 0, warning: false, exhausted: false, warningThresholdPct: 90 } },
      ]),
    },
  },
}));

describe("ResourcesPage", () => {
  it("renders vtl list and selected vtl resources", async () => {
    renderWithProviders(<ResourcesPage />);
    expect(await screen.findByRole("heading", { name: "Resource Management", level: 1 })).toBeInTheDocument();
    expect(await screen.findByRole("row", { name: /Lib A/ })).toHaveClass("clickable-table-row");
    expect(screen.queryByRole("button", { name: "Manage" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Delete" })).not.toBeInTheDocument();
    expect(await screen.findByRole("button", { name: "Create VTL" })).toBeInTheDocument();
  });
});
