import { act, cleanup, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { renderWithProviders } from "../test/renderWithProviders";
import { api } from "../services/api";
import { TargetsPage } from "./TargetsPage";
import type { LocalMountStatus } from "../services/types";

const iscsiSecurityMock = vi.hoisted(() => ({
  listTargets: vi.fn().mockResolvedValue([{ binding: { targetIqn: "iqn.2026-01.example:offline", ownerId: "iqn.2026-01.example:offline", scope: "target", deviceRole: "drive", generation: 1 }, resolved: { auth: { mode: "none" }, authSource: { scope: "default" }, effectiveRevision: "rev" } }]),
  getTargetBinding: vi.fn().mockResolvedValue({ binding: { targetIqn: "iqn.2026-01.example:offline", ownerId: "iqn.2026-01.example:offline", scope: "target", deviceRole: "drive", generation: 1 }, resolved: { auth: { mode: "none" }, authSource: { scope: "default" }, effectiveRevision: "rev" } }),
  listCredentials: vi.fn().mockResolvedValue([]),
}));

vi.mock("../services/api", () => ({
  api: {
    targets: {
      listPublications: vi.fn().mockResolvedValue([]),
      localMountStatus: vi.fn().mockResolvedValue({ enabled: false, state: "disabled", desiredDeviceCount: 0, connectedDeviceCount: 0, residualDeviceCount: 0, devices: [] }),
      setLocalMount: vi.fn().mockResolvedValue({ enabled: true, state: "connecting", desiredDeviceCount: 3, connectedDeviceCount: 0, residualDeviceCount: 0, devices: [] }),
      unpublish: vi.fn().mockResolvedValue({}),
      createPublication: vi.fn().mockResolvedValue({}),
    },
    iscsiSecurity: iscsiSecurityMock,
  },
}));

const disabledMount: LocalMountStatus = { enabled: false, state: "disabled", desiredDeviceCount: 0, connectedDeviceCount: 0, residualDeviceCount: 0, devices: [] };
const connectedMount: LocalMountStatus = { ...disabledMount, enabled: true, state: "connected", desiredDeviceCount: 3, connectedDeviceCount: 3 };
const connectingMount: LocalMountStatus = { ...connectedMount, state: "connecting", connectedDeviceCount: 0 };

beforeEach(() => {
  vi.clearAllMocks();
  vi.mocked(api.targets.listPublications).mockResolvedValue([]);
  vi.mocked(api.targets.localMountStatus).mockResolvedValue(disabledMount);
  vi.mocked(api.targets.setLocalMount).mockResolvedValue(connectingMount);
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

describe("TargetsPage security inventory", () => {
  it("keeps an offline stable target editable", async () => {
    renderWithProviders(<TargetsPage />);
    expect(await screen.findByText("iqn.2026-01.example:offline")).toBeInTheDocument();
    expect(screen.queryByText("Protected targets skipped from local mounting")).not.toBeInTheDocument();
    expect(screen.queryByText(/CHAP target/)).not.toBeInTheDocument();
    await userEvent.click(await screen.findByRole("button", { name: "Actions for iqn.2026-01.example:offline" }));
    await userEvent.click(within(screen.getByRole("menu")).getByRole("menuitem", { name: "Set CHAP" }));
    expect(await screen.findByRole("dialog")).toBeInTheDocument();
    expect(await screen.findByRole("heading", { name: "Configure CHAP" })).toBeInTheDocument();
    expect(iscsiSecurityMock.getTargetBinding).toHaveBeenCalledWith("iqn.2026-01.example:offline");
  });

  it("does not show the number of locally mounted devices", async () => {
    vi.mocked(api.targets.localMountStatus).mockResolvedValue({
      enabled: true, state: "partial", desiredDeviceCount: 3, connectedDeviceCount: 2, residualDeviceCount: 0,
      devices: [{
        deviceKey: "drive:drive-c", kind: "drive", libraryId: "lib-a", driveId: "drive-c",
        displayName: "Library A / drive-c", state: "not_ready", observedPaths: [], reasonCode: "pool_unavailable",
      }],
    });
    renderWithProviders(<TargetsPage />);
    await screen.findByText("iqn.2026-01.example:offline");
    expect(screen.queryByRole("button", { name: /Connected 2\/3/ })).not.toBeInTheDocument();
  });

  it("requires confirmation before taking an active target offline", async () => {
    vi.mocked(api.targets.listPublications).mockResolvedValue([{
      publicationId: "pub-a", poolId: "pool-a", libraryId: "lib-a", driveId: "drive-a", cartridgeId: "cart-a",
      targetIqn: "iqn.2026-01.example:offline", deviceRole: "drive", portal: "192.0.2.10:3260", state: "ready",
      compressionEnabled: false, dedupEnabled: false, createdAt: "2026-01-01T00:00:00Z", updatedAt: "2026-01-01T00:00:00Z",
    }]);
    renderWithProviders(<TargetsPage />);
    await userEvent.click(await screen.findByRole("button", { name: "Actions for iqn.2026-01.example:offline" }));
    const takeOffline = within(screen.getByRole("menu")).getByRole("menuitem", { name: "Take offline" });
    await waitFor(() => expect(takeOffline).toBeEnabled());
    await userEvent.click(takeOffline);
    const dialog = await screen.findByRole("dialog", { name: "Take target offline?" });
    expect(dialog).toHaveTextContent("disconnects any active backup-host sessions");
    await userEvent.click(within(dialog).getByRole("button", { name: "Take offline" }));

    expect(api.targets.unpublish).toHaveBeenCalledWith("pub-a");
  });

  it("offers an explicit bring-online action for an offline target", async () => {
    vi.mocked(api.targets.listPublications).mockResolvedValue([{
      publicationId: "pub-a", poolId: "pool-a", libraryId: "lib-a", driveId: "drive-a", cartridgeId: "cart-a",
      targetIqn: "iqn.2026-01.example:offline", deviceRole: "drive", portal: "192.0.2.10:3260", state: "disabled",
      compressionEnabled: false, dedupEnabled: false, createdAt: "2026-01-01T00:00:00Z", updatedAt: "2026-01-01T00:00:00Z",
    }]);
    renderWithProviders(<TargetsPage />);
    await userEvent.click(await screen.findByRole("button", { name: "Actions for iqn.2026-01.example:offline" }));
    const bringOnline = within(screen.getByRole("menu")).getByRole("menuitem", { name: "Bring online" });
    await waitFor(() => expect(bringOnline).toBeEnabled());
    await userEvent.click(bringOnline);
    const dialog = await screen.findByRole("dialog", { name: "Bring target online?" });
    expect(dialog).toHaveTextContent("without matching CHAP settings may not connect");
    await userEvent.click(within(dialog).getByRole("button", { name: "Bring online" }));

    expect(api.targets.createPublication).toHaveBeenCalledWith(expect.objectContaining({
      libraryId: "lib-a", driveId: "drive-a", cartridgeId: "cart-a", targetIqn: "iqn.2026-01.example:offline",
      deviceRole: "drive", actor: "web-console",
    }));
  });
});

describe("TargetsPage local mount feedback", () => {
  async function renderPage() {
    vi.useFakeTimers();
    await act(async () => { renderWithProviders(<TargetsPage />); });
  }

  async function click(element: HTMLElement) {
    await act(async () => { element.click(); });
  }

  async function poll() {
    await act(async () => { await vi.advanceTimersByTimeAsync(2500); });
  }

  it.each([null, undefined])("renders a disabled mount with a %s device list returned by the server", async (devices) => {
    vi.mocked(api.targets.localMountStatus).mockResolvedValue({ ...disabledMount, devices: devices as unknown as LocalMountStatus["devices"] });
    await renderPage();
    expect(screen.getByRole("status")).toHaveTextContent("Not mounted");
    expect(screen.getByRole("checkbox", { name: "Mount Locally" })).toBeEnabled();
    expect(screen.getByRole("checkbox", { name: "Mount Locally" })).not.toBeChecked();
    expect(screen.getByText("iqn.2026-01.example:offline")).toBeInTheDocument();
  });

  it("keeps the page visible while disabling with null device lists", async () => {
    vi.mocked(api.targets.localMountStatus).mockResolvedValue(connectedMount);
    vi.mocked(api.targets.setLocalMount).mockResolvedValue({ ...disabledMount, state: "disconnecting", devices: null as unknown as LocalMountStatus["devices"] });
    await renderPage();
    const toggle = screen.getByRole("checkbox", { name: "Mount Locally" });
    await click(toggle);
    expect(api.targets.setLocalMount).toHaveBeenCalledWith(false);
    expect(toggle).toBeDisabled();
    expect(screen.getByRole("status")).toHaveTextContent("Unmounting…");
    vi.mocked(api.targets.localMountStatus).mockResolvedValue({ ...disabledMount, devices: null as unknown as LocalMountStatus["devices"] });
    await poll();
    expect(toggle).toBeEnabled();
    expect(toggle).not.toBeChecked();
    expect(screen.getByRole("status")).toHaveTextContent("Not mounted");
    expect(screen.getByText("iqn.2026-01.example:offline")).toBeInTheDocument();
  });

  it("shows an unknown state instead of a stale mounted status after a query fails", async () => {
    vi.mocked(api.targets.localMountStatus).mockResolvedValue(connectedMount);
    await renderPage();
    vi.mocked(api.targets.localMountStatus).mockRejectedValueOnce(new Error("network unavailable"));
    await poll();
    expect(screen.getByRole("status")).toHaveTextContent("Status unavailable");
    expect(screen.getByRole("checkbox", { name: "Mount Locally" })).toBeDisabled();
    await poll();
    expect(screen.getByRole("status")).toHaveTextContent("Mounted");
    expect(screen.getByRole("checkbox", { name: "Mount Locally" })).toBeEnabled();
    expect(screen.queryByText("Local mount completed")).not.toBeInTheDocument();
  });

  it("locks the switch until mounting completes and announces success only once", async () => {
    await renderPage();
    const toggle = screen.getByRole("checkbox", { name: "Mount Locally" });
    await click(toggle);
    expect(toggle).toBeDisabled();
    expect(screen.getByRole("status")).toHaveTextContent("Mounting…");
    expect(screen.queryByText("Mount request accepted")).not.toBeInTheDocument();
    expect(screen.queryByText("Local mount completed")).not.toBeInTheDocument();
    await click(toggle);
    await click(toggle);
    expect(api.targets.setLocalMount).toHaveBeenCalledTimes(1);

    vi.mocked(api.targets.localMountStatus).mockResolvedValue(connectedMount);
    await poll();
    expect(toggle).toBeEnabled();
    expect(toggle).toBeChecked();
    expect(screen.getByRole("status")).toHaveTextContent("Mounted");
    expect(screen.getAllByText("Local mount completed")).toHaveLength(1);
    await poll();
    expect(screen.getAllByText("Local mount completed")).toHaveLength(1);
  });

  it("keeps unmounting locked until cleanup is confirmed", async () => {
    vi.mocked(api.targets.localMountStatus).mockResolvedValue(connectedMount);
    vi.mocked(api.targets.setLocalMount).mockResolvedValue({ ...disabledMount, state: "disconnecting" });
    await renderPage();
    const toggle = screen.getByRole("checkbox", { name: "Mount Locally" });
    expect(screen.queryByText("Local mount completed")).not.toBeInTheDocument();
    await click(toggle);
    expect(toggle).toBeDisabled();
    expect(screen.getByRole("status")).toHaveTextContent("Unmounting…");
    expect(screen.queryByText("Local mount disabled")).not.toBeInTheDocument();
    vi.mocked(api.targets.localMountStatus).mockResolvedValue(disabledMount);
    await poll();
    expect(toggle).toBeEnabled();
    expect(toggle).not.toBeChecked();
    expect(screen.getByRole("status")).toHaveTextContent("Not mounted");
    expect(screen.getByText("Local mount disabled")).toBeInTheDocument();
  });

  it("shows a per-device failure and retries the same enabled intent", async () => {
    const partial: LocalMountStatus = {
      ...connectedMount, state: "partial", connectedDeviceCount: 2,
      devices: [{ deviceKey: "drive:drive-c", kind: "drive", libraryId: "lib-a", driveId: "drive-c", displayName: "Library A / drive-c", state: "not_ready", observedPaths: [], reasonCode: "pool_unavailable" }],
    };
    vi.mocked(api.targets.localMountStatus).mockResolvedValue(partial);
    await renderPage();
    expect(screen.getByRole("status")).toHaveTextContent("Partially mounted");
    expect(screen.getByRole("alert")).toHaveTextContent("Library A / drive-c");
    expect(screen.getByRole("alert")).toHaveTextContent("The loaded cartridge's storage pool is unavailable.");
    await click(screen.getByRole("button", { name: "Retry" }));
    expect(api.targets.setLocalMount).toHaveBeenCalledWith(true);
    expect(screen.getByRole("checkbox", { name: "Mount Locally" })).toBeDisabled();
    expect(screen.getByRole("status")).toHaveTextContent("Mounting…");
  });

  it("reports incomplete removal and retries the disabled intent", async () => {
    vi.mocked(api.targets.localMountStatus).mockResolvedValue({ ...disabledMount, state: "failed", residualDeviceCount: 1, lastError: "device_busy" });
    vi.mocked(api.targets.setLocalMount).mockResolvedValue({ ...disabledMount, state: "disconnecting" });
    await renderPage();
    expect(screen.getByRole("status")).toHaveTextContent("Unmount incomplete");
    expect(screen.getByRole("alert")).toHaveTextContent("The device is busy and cannot be removed yet.");
    await click(screen.getByRole("button", { name: "Retry" }));
    expect(api.targets.setLocalMount).toHaveBeenCalledWith(false);
    expect(screen.getByRole("status")).toHaveTextContent("Unmounting…");
  });

  it("keeps processing locked when polling fails and recovers on a later check", async () => {
    await renderPage();
    const toggle = screen.getByRole("checkbox", { name: "Mount Locally" });
    await click(toggle);
    vi.mocked(api.targets.localMountStatus).mockRejectedValueOnce(new Error("network unavailable"));
    await poll();
    expect(toggle).toBeDisabled();
    expect(screen.getByRole("status")).toHaveTextContent("Status unavailable");
    expect(screen.getByRole("alert")).toHaveTextContent("Unable to check mount status. Retrying…");
    vi.mocked(api.targets.localMountStatus).mockResolvedValue(connectedMount);
    await click(screen.getByRole("button", { name: "Check status again" }));
    expect(toggle).toBeEnabled();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(screen.getByText("Local mount completed")).toBeInTheDocument();
  });

  it("ignores a status query started before a newer unmount request", async () => {
    vi.mocked(api.targets.localMountStatus).mockResolvedValue(connectedMount);
    await renderPage();
    let resolveOldStatus!: (status: LocalMountStatus) => void;
    vi.mocked(api.targets.localMountStatus).mockImplementationOnce(() => new Promise((resolve) => { resolveOldStatus = resolve; }));
    await poll();
    vi.mocked(api.targets.setLocalMount).mockResolvedValue({ ...disabledMount, state: "disconnecting" });
    await click(screen.getByRole("checkbox", { name: "Mount Locally" }));
    await act(async () => { resolveOldStatus(connectedMount); });
    expect(screen.getByRole("status")).toHaveTextContent("Unmounting…");
    expect(screen.getByRole("checkbox", { name: "Mount Locally" })).toBeDisabled();
    vi.mocked(api.targets.localMountStatus).mockResolvedValue(disabledMount);
    await poll();
    expect(screen.getByRole("status")).toHaveTextContent("Not mounted");
  });
});
