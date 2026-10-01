import { cleanup, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { renderWithProviders } from "../test/renderWithProviders";
import { api } from "../services/api";
import { TargetsPage } from "./TargetsPage";

const iscsiSecurityMock = vi.hoisted(() => ({
  listTargets: vi.fn().mockResolvedValue([{ binding: { targetIqn: "iqn.2026-01.example:offline", ownerId: "iqn.2026-01.example:offline", scope: "target", deviceRole: "drive", generation: 1 }, resolved: { auth: { mode: "none" }, authSource: { scope: "default" }, effectiveRevision: "rev" } }]),
  getTargetBinding: vi.fn().mockResolvedValue({ binding: { targetIqn: "iqn.2026-01.example:offline", ownerId: "iqn.2026-01.example:offline", scope: "target", deviceRole: "drive", generation: 1 }, resolved: { auth: { mode: "none" }, authSource: { scope: "default" }, effectiveRevision: "rev" } }),
  listCredentials: vi.fn().mockResolvedValue([]),
}));

vi.mock("../services/api", () => ({
  api: {
    targets: {
      listPublications: vi.fn().mockResolvedValue([]),
      localMountStatus: vi.fn().mockResolvedValue({ enabled: false, desiredIqns: [], mountedIqns: [], skippedTargets: [{ targetIqn: "iqn.2026-01.example:offline", reason: "CHAP target" }] }),
      unpublish: vi.fn().mockResolvedValue({}),
      createPublication: vi.fn().mockResolvedValue({}),
    },
    iscsiSecurity: iscsiSecurityMock,
  },
}));

afterEach(() => cleanup());

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
