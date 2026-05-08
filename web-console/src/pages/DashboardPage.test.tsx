import { screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { renderWithProviders } from "../test/renderWithProviders";
import { DashboardPage } from "./DashboardPage";

vi.mock("../services/api", () => ({
  api: {
    ops: {
      health: vi.fn().mockResolvedValue({ status: "healthy" }),
      systemOverview: vi.fn().mockResolvedValue({
        hostname: "holo-dev",
        collectedAt: new Date().toISOString(),
        uptimeSeconds: 3600,
        cpuLoad1m: 0,
        cpuLoad5m: 0,
        cpuLoad15m: 0,
        memoryTotalBytes: 1000,
        memoryAvailableBytes: 500,
        networkRxBytes: 100,
        networkTxBytes: 50,
        iscsiSessionCount: 1,
      }),
      cdbTrace: vi.fn().mockRejectedValue(new Error("not found")),
      setCDBTrace: vi.fn(),
      supportBundle: vi.fn(),
    },
    storage: {
      listPools: vi.fn().mockResolvedValue([
        {
          poolId: "pool1",
          name: "mypool",
          status: "active",
          disks: [],
          capacity: {
            totalBytes: 1000,
            usedBytes: 10,
            freeBytes: 990,
            usedPercent: 1,
            warning: false,
            exhausted: false,
            warningThresholdPct: 90,
          },
        },
      ]),
    },
    resources: {
      listLibraries: vi.fn().mockResolvedValue([]),
      listCartridges: vi.fn().mockResolvedValue([]),
    },
    targets: {
      listPublications: vi.fn().mockResolvedValue([]),
    },
  },
}));

describe("DashboardPage", () => {
  it("does not show a partial data notice when optional CDB trace is unavailable", async () => {
    renderWithProviders(<DashboardPage />);

    expect(await screen.findByRole("heading", { name: "System Overview", level: 1 })).toBeInTheDocument();
    await waitFor(() => {
      expect(screen.queryByText(/partial metrics/i)).not.toBeInTheDocument();
    });
  });
});
