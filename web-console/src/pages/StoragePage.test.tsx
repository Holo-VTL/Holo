import { screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { renderWithProviders } from "../test/renderWithProviders";
import { StoragePage } from "./StoragePage";

vi.mock("../services/api", () => ({
  api: {
    storage: {
      discoverDisks: vi.fn().mockResolvedValue([
        { devicePath: "/dev/sdb", sizeBytes: 1000, availability: "available" },
      ]),
      listPools: vi.fn().mockResolvedValue([
        {
          poolId: "pool-a",
          name: "pool-a",
          status: "active",
          warningThresholdPct: 90,
          disks: [{ devicePath: "/dev/sdb", sizeBytes: 1000, attachedAt: new Date().toISOString() }],
          capacity: { totalBytes: 1000, usedBytes: 0, freeBytes: 1000, usedPercent: 0, warning: false, exhausted: false, warningThresholdPct: 90 },
        },
      ]),
      createPool: vi.fn(),
      deletePool: vi.fn(),
      attachDisk: vi.fn(),
      detachDisk: vi.fn(),
    },
  },
}));

describe("StoragePage", () => {
  it("renders storage contract flow", async () => {
    renderWithProviders(<StoragePage />);
    expect(await screen.findByText("Storage Management")).toBeInTheDocument();
    expect(await screen.findByText("/dev/sdb")).toBeInTheDocument();
    expect(await screen.findByRole("button", { name: "Delete" })).toBeEnabled();
  });
});
