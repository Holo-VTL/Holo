import { cleanup, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { renderWithProviders } from "../test/renderWithProviders";
import { api } from "../services/api";
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
    iscsiSecurity: {
      listCredentials: vi.fn().mockResolvedValue([]),
      putLibraryBinding: vi.fn().mockResolvedValue({ binding: {} }),
    },
  },
}));

afterEach(() => cleanup());

describe("ResourcesPage", () => {
  it("renders vtl list and selected vtl resources", async () => {
    renderWithProviders(<ResourcesPage />);
    expect(await screen.findByRole("heading", { name: "Resource Management", level: 1 })).toBeInTheDocument();
    expect(await screen.findByRole("row", { name: /Lib A/ })).toHaveClass("clickable-table-row");
    expect(screen.queryByRole("button", { name: "Manage" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Delete" })).not.toBeInTheDocument();
    expect(await screen.findByRole("button", { name: "Create VTL" })).toBeInTheDocument();
  });

  it("keeps CHAP changes as a draft until Create submits them", async () => {
    vi.mocked(api.resources.createLibrary).mockClear();
    vi.mocked(api.resources.createDrive).mockClear();
    vi.mocked(api.iscsiSecurity.putLibraryBinding).mockClear();
    vi.mocked(api.iscsiSecurity.listCredentials).mockResolvedValue([
      { credentialId: "chap-a", label: "Backup host", username: "backup", version: 1, createdAt: "", secretPresent: true },
    ]);
    const user = userEvent.setup();
    renderWithProviders(<ResourcesPage />);
    await user.click((await screen.findAllByRole("button", { name: "Create VTL" }))[0]);
    expect(screen.getByRole("button", { name: "Config CHAP" })).toBeInTheDocument();
    await user.type(screen.getByLabelText("VTL Name"), "Backup Lab");
    await user.click(screen.getByRole("button", { name: "Config CHAP" }));
    expect(screen.getByRole("dialog", { name: "Configure CHAP" })).toBeInTheDocument();
    expect(screen.getAllByRole("dialog")).toHaveLength(1);
    await waitFor(() => expect(api.iscsiSecurity.listCredentials).toHaveBeenCalled());
    await user.click(screen.getByRole("button", { name: "Login check" }));
    await user.click(await screen.findByRole("option", { name: "One-way CHAP" }));
    await user.click(screen.getByRole("button", { name: "CHAP login credentials" }));
    await user.click(await screen.findByRole("option", { name: "Backup host · backup" }));
    await user.type(screen.getByLabelText("Allowed backup host IQNs (one per line)"), "iqn.1991-05.com.microsoft:backup-host");
    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(screen.getByRole("dialog", { name: "Create Virtual Library" })).toBeInTheDocument();
    expect(screen.getByText("Configured")).toBeInTheDocument();
    expect(api.iscsiSecurity.putLibraryBinding).not.toHaveBeenCalled();
    await user.click(screen.getByRole("button", { name: "Create" }));

    await waitFor(() => expect(api.iscsiSecurity.putLibraryBinding).toHaveBeenCalledWith(expect.any(String), {
      generation: 1,
      auth: { mode: "chap", credentialId: "chap-a", initiators: ["iqn.1991-05.com.microsoft:backup-host"] },
      actor: "web-console",
    }));
    expect(vi.mocked(api.resources.createLibrary).mock.invocationCallOrder[0]).toBeLessThan(
      vi.mocked(api.iscsiSecurity.putLibraryBinding).mock.invocationCallOrder[0],
    );
  });

  it("checks CHAP selections before creating the Library", async () => {
    vi.mocked(api.resources.createLibrary).mockClear();
    vi.mocked(api.iscsiSecurity.putLibraryBinding).mockClear();
    vi.mocked(api.iscsiSecurity.listCredentials).mockResolvedValue([]);
    const user = userEvent.setup();
    renderWithProviders(<ResourcesPage />);
    await user.click((await screen.findAllByRole("button", { name: "Create VTL" }))[0]);
    await user.type(screen.getByLabelText("VTL Name"), "Backup Lab");
    await user.click(screen.getByRole("button", { name: "Config CHAP" }));
    await screen.findByRole("dialog", { name: "Configure CHAP" });
    await user.click(screen.getByRole("button", { name: "Login check" }));
    await user.click(await screen.findByRole("option", { name: "One-way CHAP" }));
    await user.type(screen.getByLabelText("Allowed backup host IQNs (one per line)"), "iqn.1991-05.com.microsoft:backup-host");
    await user.click(screen.getByRole("button", { name: "Save" }));
    await user.click(screen.getByRole("button", { name: "Create" }));

    expect(await screen.findByText("Select a CHAP credential before saving.")).toBeInTheDocument();
    expect(screen.getByRole("dialog", { name: "Configure CHAP" })).toBeInTheDocument();
    expect(api.resources.createLibrary).not.toHaveBeenCalled();
  });

  it("discards CHAP edits when the editor is canceled", async () => {
    vi.mocked(api.iscsiSecurity.listCredentials).mockResolvedValue([
      { credentialId: "chap-a", label: "Backup host", username: "backup", version: 1, createdAt: "", secretPresent: true },
    ]);
    const user = userEvent.setup();
    renderWithProviders(<ResourcesPage />);
    await user.click((await screen.findAllByRole("button", { name: "Create VTL" }))[0]);
    await user.click(screen.getByRole("button", { name: "Config CHAP" }));
    await user.click(screen.getByRole("button", { name: "Login check" }));
    await user.click(await screen.findByRole("option", { name: "One-way CHAP" }));
    await user.click(screen.getByRole("button", { name: "Cancel" }));

    expect(screen.getByRole("dialog", { name: "Create Virtual Library" })).toBeInTheDocument();
    expect(screen.getAllByText("Not configured")).toHaveLength(1);
    await user.click(screen.getByRole("button", { name: "Config CHAP" }));
    expect(screen.getByRole("button", { name: "Login check" })).toHaveTextContent("Do not use CHAP");
  });

  it("returns to Create VTL when the CHAP editor close button is clicked", async () => {
    const user = userEvent.setup();
    renderWithProviders(<ResourcesPage />);
    await user.click((await screen.findAllByRole("button", { name: "Create VTL" }))[0]);
    await user.click(screen.getByRole("button", { name: "Config CHAP" }));

    expect(screen.getByRole("dialog", { name: "Configure CHAP" })).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Close" }));

    expect(screen.getByRole("dialog", { name: "Create Virtual Library" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Config CHAP" })).toBeInTheDocument();
  });

});
