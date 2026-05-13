import { screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { renderWithProviders } from "../test/renderWithProviders";
import { TargetsPage } from "./TargetsPage";

vi.mock("../services/api", () => ({
  api: {
    targets: {
      listPublications: vi.fn().mockResolvedValue([
        {
          publicationId: "pub-a",
          targetIqn: "iqn.a",
          deviceRole: "drive",
          portal: "10.0.0.1:3260",
          state: "ready",
          connectedHosts: {
            available: true,
            hostCount: 2,
            sessionCount: 2,
            initiators: ["iqn.1993-08.org.debian:01:init-a", "iqn.1991-05.com.microsoft:host-a"],
          },
        },
        {
          publicationId: "pub-empty",
          targetIqn: "iqn.empty",
          deviceRole: "drive",
          portal: "10.0.0.1:3260",
          state: "ready",
          connectedHosts: { available: true, hostCount: 0, sessionCount: 0, initiators: [] },
        },
        {
          publicationId: "pub-unknown",
          targetIqn: "iqn.unknown",
          deviceRole: "drive",
          portal: "10.0.0.1:3260",
          state: "ready",
          connectedHosts: { available: false, hostCount: 0, sessionCount: 0, initiators: [], lastError: "session discovery unavailable" },
        },
        { publicationId: "pub-b", targetIqn: "iqn.b", deviceRole: "changer", portal: "10.0.0.1:3260", state: "disabled" },
      ]),
      localMountStatus: vi.fn().mockResolvedValue({ enabled: false, desiredIqns: [], mountedIqns: [] }),
      setLocalMount: vi.fn(),
      createPublication: vi.fn(),
      unpublish: vi.fn(),
      rollback: vi.fn(),
      listValidationRuns: vi.fn().mockResolvedValue([]),
      startValidationRun: vi.fn(),
    },
    resources: {
      listLibraries: vi.fn().mockResolvedValue([{ libraryId: "lib-a", name: "Lib A" }]),
      listDrives: vi.fn().mockResolvedValue([{ driveId: "drive-a", libraryId: "lib-a", slot: 1 }]),
      listCartridges: vi.fn().mockResolvedValue([{ cartridgeId: "car-a", poolId: "pool-a", libraryId: "lib-a", barcode: "VTA000L06", capacityBytes: 1000 }]),
    },
  },
}));

describe("TargetsPage", () => {
  it("renders only active publication rows", async () => {
    renderWithProviders(<TargetsPage />);
    expect(await screen.findByRole("heading", { name: "Target Publications", level: 1 })).toBeInTheDocument();
    expect(await screen.findByRole("cell", { name: "iqn.a" })).toBeInTheDocument();
    expect(screen.getByRole("columnheader", { name: "Connected Hosts" })).toBeInTheDocument();
    expect(screen.getAllByRole("cell", { name: "drive" })).toHaveLength(3);
    expect(screen.getByText("2 hosts")).toBeInTheDocument();
    expect(screen.queryByText(/iqn\.1993-08\.org\.debian:01:init-a/)).not.toBeInTheDocument();
    expect(screen.getByText("2 hosts").closest(".connected-hosts-cell")).toHaveAttribute(
      "title",
      "iqn.1993-08.org.debian:01:init-a\niqn.1991-05.com.microsoft:host-a"
    );
    expect(screen.getByText("No sessions")).toBeInTheDocument();
    expect(screen.getByText("Unavailable")).toBeInTheDocument();
    expect(screen.queryByRole("cell", { name: "iqn.b" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Unpublish" })).not.toBeInTheDocument();
  });
});
