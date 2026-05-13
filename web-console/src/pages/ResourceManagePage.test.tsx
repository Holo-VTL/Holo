import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { describe, expect, it, vi } from "vitest";
import { ThemeProvider } from "../app/ThemeContext";
import { ToastProvider } from "../components/Toast";
import "../i18n";
import { ResourceManagePage } from "./ResourceManagePage";

vi.mock("../services/api", () => ({
  api: {
    resources: {
      listLibraries: vi.fn().mockResolvedValue([{ libraryId: "lib-a", name: "Lib A", status: "online", slotCount: 12, slotStartAddress: 1024, driveType: "IBM-LTO6" }]),
      listDrives: vi.fn().mockResolvedValue([{ driveId: "drive-a", libraryId: "lib-a", slot: 1, mountState: "empty" }]),
      listCartridges: vi.fn().mockResolvedValue([{ cartridgeId: "VTA000L06", poolId: "pool-a", libraryId: "lib-a", barcode: "VTA000L06", capacityBytes: 1000, usedBytes: 100, lifecycleState: "available", retentionState: "none", currentElementAddress: 1034 }]),
      createLibrary: vi.fn(),
      createDrive: vi.fn(),
      createCartridge: vi.fn(),
      deleteLibrary: vi.fn(),
      deleteDrive: vi.fn(),
      deleteCartridge: vi.fn(),
      eraseCartridge: vi.fn(),
      exportCartridge: vi.fn(),
      importCartridge: vi.fn(),
      loadCartridge: vi.fn(),
      unloadDrive: vi.fn(),
    },
    storage: {
      listPools: vi.fn().mockResolvedValue([{ poolId: "pool-a", name: "Pool A", status: "active", disks: [{ devicePath: "/dev/sdb", sizeBytes: 1000, attachedAt: new Date().toISOString() }], capacity: { totalBytes: 1000, usedBytes: 0, freeBytes: 1000, usedPercent: 0, warning: false, exhausted: false, warningThresholdPct: 90 } }]),
    },
  },
}));

function renderManagePage() {
  return render(
    <MemoryRouter initialEntries={["/resources/lib-a"]}>
      <ThemeProvider>
        <ToastProvider>
          <Routes>
            <Route path="/resources/:libraryId" element={<ResourceManagePage />} />
          </Routes>
        </ToastProvider>
      </ThemeProvider>
    </MemoryRouter>
  );
}

describe("ResourceManagePage", () => {
  it("shows erase actions and destroy wording for selected cartridge", async () => {
    renderManagePage();
    await userEvent.click(await screen.findByText("VTA000L06"));
    expect(await screen.findByRole("button", { name: "Short Erase" })).toBeEnabled();
    expect(await screen.findByRole("button", { name: "Long Erase" })).toBeEnabled();
    expect(await screen.findByRole("button", { name: "Destroy Cartridge" })).toBeEnabled();
    await userEvent.click(screen.getByRole("button", { name: "Short Erase" }));
    expect(await screen.findByText(/Existing tape contents will be cleared/)).toBeInTheDocument();
  });

  it("places cartridges by reported element address", async () => {
    renderManagePage();
    const cartridgeSlot = await screen.findByTitle("VTA000L06 (VTA000L06)");
    expect(cartridgeSlot).toHaveAttribute("data-slot-address", "1034");
  });
});
