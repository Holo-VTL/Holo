import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ThemeProvider } from "../app/ThemeContext";
import { ToastProvider } from "../components/Toast";
import "../i18n";
import { api } from "../services/api";
import { ResourceManagePage } from "./ResourceManagePage";

vi.mock("../services/api", () => ({
  api: {
    resources: {
      listLibraries: vi.fn().mockResolvedValue([{ libraryId: "lib-a", name: "Lib A", status: "online", slotCount: 12, slotStartAddress: 1024, driveType: "IBM-LTO6" }]),
      listDrives: vi.fn().mockResolvedValue([{ driveId: "drive-a", libraryId: "lib-a", slot: 1, mountState: "empty" }]),
      listCartridges: vi.fn().mockResolvedValue([{ cartridgeId: "VTA000L06", poolId: "pool-a", libraryId: "lib-a", barcode: "VTA000L06", capacityBytes: 1000, usedBytes: 100, lifecycleState: "available", retentionState: "none", currentElementAddress: 1034 }]),
      createLibrary: vi.fn(),
      addLibrarySlots: vi.fn().mockResolvedValue({ libraryId: "lib-a", name: "Lib A", status: "online", slotCount: 13, slotStartAddress: 1024, driveType: "IBM-LTO6" }),
      createDrive: vi.fn(),
      createCartridge: vi.fn().mockResolvedValue({ cartridgeId: "VTA001L06", poolId: "pool-a", libraryId: "lib-a", barcode: "VTA001L06", capacityBytes: 1000, usedBytes: 0, lifecycleState: "available", retentionState: "none" }),
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
  afterEach(() => {
    cleanup();
    vi.clearAllMocks();
  });

  it("separates slot expansion from cartridge creation", async () => {
    renderManagePage();
    const addSlot = await screen.findByRole("button", { name: "Add Slot" });
    expect(addSlot).toBeEnabled();
    expect(await screen.findByRole("button", { name: "Add Cartridge" })).toBeEnabled();
    await userEvent.click(addSlot);
    expect(api.resources.addLibrarySlots).toHaveBeenCalledWith("lib-a", { count: 1, actor: "web-console" });
  });

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

  it("does not place cartridges without a reported slot address", async () => {
    vi.mocked(api.resources.listCartridges).mockResolvedValue([
      {
        cartridgeId: "VTA000L06",
        poolId: "pool-a",
        libraryId: "lib-a",
        barcode: "VTA000L06",
        capacityBytes: 1000,
        usedBytes: 100,
        lifecycleState: "available",
        retentionState: "none",
        currentElementAddress: 1034,
        createdAt: new Date().toISOString(),
        updatedAt: new Date().toISOString(),
      },
      {
        cartridgeId: "VTA031L06",
        poolId: "pool-a",
        libraryId: "lib-a",
        barcode: "VTA031L06",
        capacityBytes: 1000,
        usedBytes: 0,
        lifecycleState: "available",
        retentionState: "none",
        createdAt: new Date().toISOString(),
        updatedAt: new Date().toISOString(),
      },
    ]);
    renderManagePage();
    expect(await screen.findByTitle("VTA000L06 (VTA000L06)")).toBeInTheDocument();
    expect(screen.queryByText("VTA031L06")).not.toBeInTheDocument();
  });

  it("requires explicit slot expansion when creating more cartridges than empty slots", async () => {
    renderManagePage();
    const addCartridgeButtons = await screen.findAllByRole("button", { name: "Add Cartridge" });
    await userEvent.click(addCartridgeButtons[0]);
    const quantity = await screen.findByLabelText("Cartridge Count");
    await userEvent.clear(quantity);
    await userEvent.type(quantity, "12");
    expect(await screen.findByText(/does not have enough empty slots/)).toBeInTheDocument();
    await userEvent.click(screen.getByLabelText(/Add slot and insert cartridge/));
    await userEvent.click(screen.getByRole("button", { name: "Create" }));
    expect(api.resources.createCartridge).toHaveBeenCalledWith(
      expect.objectContaining({ barcodePrefix: "VTA", expandSlots: true })
    );
    const calls = vi.mocked(api.resources.createCartridge).mock.calls;
    const request = calls[calls.length - 1]?.[0];
    expect(request).not.toHaveProperty("barcode");
    expect(request).not.toHaveProperty("cartridgeId");
  });

  it("offers to add a slot when vault import has no empty slot", async () => {
    vi.mocked(api.resources.listCartridges).mockResolvedValue([
      {
        cartridgeId: "VTA000L06",
        poolId: "pool-a",
        libraryId: "lib-a",
        barcode: "VTA000L06",
        capacityBytes: 1000,
        usedBytes: 100,
        lifecycleState: "exported",
        retentionState: "none",
        createdAt: new Date().toISOString(),
        updatedAt: new Date().toISOString(),
      },
    ]);
    vi.mocked(api.resources.importCartridge)
      .mockRejectedValueOnce(Object.assign(new Error("conflict"), { status: 409 }))
      .mockResolvedValueOnce({
        cartridgeId: "VTA000L06",
        poolId: "pool-a",
        libraryId: "lib-a",
        barcode: "VTA000L06",
        capacityBytes: 1000,
        usedBytes: 100,
        lifecycleState: "available",
        retentionState: "none",
        currentElementAddress: 1035,
        assignedSlotAddress: 1035,
        createdAt: new Date().toISOString(),
        updatedAt: new Date().toISOString(),
      });

    renderManagePage();
    await userEvent.click(await screen.findByText("VTA000L06"));
    await userEvent.click(await screen.findByRole("button", { name: "Import from Vault" }));

    expect(await screen.findByRole("dialog")).toBeInTheDocument();
    expect(await screen.findByText("No empty slots")).toBeInTheDocument();
    expect(screen.getByText(/move another cartridge to Vault/)).toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: "Add Slot and Import" }));
    expect(api.resources.addLibrarySlots).toHaveBeenCalledWith("lib-a", { count: 1, actor: "web-console" });
    expect(api.resources.importCartridge).toHaveBeenCalledTimes(2);
  });
});
