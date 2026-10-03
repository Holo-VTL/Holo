import { cleanup, fireEvent, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { renderWithProviders } from "../test/renderWithProviders";
import { IscsiSecurityForm } from "./IscsiSecurityForm";

const securityMock = vi.hoisted(() => ({
  getLibraryBinding: vi.fn(),
  previewLibraryBinding: vi.fn(),
  putLibraryBinding: vi.fn(),
  getDriveBinding: vi.fn(),
  previewDriveBinding: vi.fn(),
  putDriveBinding: vi.fn(),
  getTargetBinding: vi.fn(),
  previewTargetBinding: vi.fn(),
  putTargetBinding: vi.fn(),
  listCredentials: vi.fn(),
}));

const targetMock = vi.hoisted(() => ({
  listPublications: vi.fn(),
  unpublish: vi.fn(),
  createPublication: vi.fn(),
}));

vi.mock("../services/api", () => ({ api: { iscsiSecurity: securityMock, targets: targetMock } }));

async function openEditor() {
  const button = await screen.findByRole("button", { name: "Set protection" });
  await waitFor(() => expect(button).toBeEnabled());
  await userEvent.click(button);
  await screen.findByRole("dialog");
}

async function chooseOption(label: string, option: string | RegExp) {
  await userEvent.click(screen.getByRole("button", { name: label }));
  await userEvent.click(await screen.findByRole("option", { name: option }));
}

const loadedBinding = {
  scope: "library" as const,
  ownerId: "lib-a",
  generation: 0,
  auth: null,
};

describe("IscsiSecurityForm", () => {
  afterEach(() => cleanup());

  beforeEach(() => {
    vi.clearAllMocks();
    securityMock.getLibraryBinding.mockResolvedValue({
      binding: loadedBinding,
      targets: [{
        binding: { ...loadedBinding, targetIqn: "iqn.2026-01.example:holo", deviceRole: "drive" },
        resolved: {
          auth: { mode: "none" },
          authSource: { scope: "default" },
          effectiveRevision: "rev-a",
        },
      }],
    });
    securityMock.listCredentials.mockResolvedValue([]);
    targetMock.listPublications.mockResolvedValue([]);
    targetMock.unpublish.mockResolvedValue({ publicationId: "pub-a", state: "disabled" });
    securityMock.previewLibraryBinding.mockResolvedValue({ targets: [{
      targetIqn: "iqn.2026-01.example:holo", changed: true, busy: false, administrativeOffline: true, absent: true,
      before: { auth: { mode: "none" }, authSource: { scope: "default" }, effectiveRevision: "before" },
      after: { auth: { mode: "chap" }, authSource: { scope: "library" }, effectiveRevision: "after" },
    }] });
    securityMock.putLibraryBinding.mockResolvedValue({ binding: { ...loadedBinding, generation: 1 } });
  });

  it("shows inherited authentication sources and saves after confirmation", async () => {
    renderWithProviders(<IscsiSecurityForm scope="library" ownerId="lib-a" title="Library policy" />);
    await openEditor();
    expect(await screen.findByText("Current settings on affected targets (1)")).toBeInTheDocument();
    await userEvent.click(screen.getByText("Current settings on affected targets (1)"));
    expect(await screen.findByText(/Login check: CHAP off \(System default\)/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await screen.findByText("Protection changes to apply");
    expect(securityMock.previewLibraryBinding).toHaveBeenCalledWith("lib-a", {
      generation: 1,
      auth: null,
          actor: "web-console",
    });
    fireEvent.click(screen.getByRole("button", { name: "Confirm and save" }));
    await waitFor(() => expect(securityMock.putLibraryBinding).toHaveBeenCalledWith("lib-a", {
      generation: 1,
      auth: null,
          actor: "web-console",
    }));
  });

  it("restarts only the targets that were online after the administrator confirms", async () => {
    const credential = { credentialId: "chap-a", label: "Backup host", username: "backup", version: 1, createdAt: "", secretPresent: true };
    securityMock.listCredentials.mockResolvedValue([credential]);
    securityMock.previewLibraryBinding.mockResolvedValue({ targets: [
      {
        targetIqn: "iqn.2026-01.example:holo-online", changed: true, busy: true, administrativeOffline: false, absent: false,
        before: { auth: { mode: "none" }, authSource: { scope: "default" }, effectiveRevision: "before" },
        after: { auth: { mode: "chap" }, authSource: { scope: "library" }, effectiveRevision: "after" },
      },
      {
        targetIqn: "iqn.2026-01.example:holo-offline", changed: true, busy: false, administrativeOffline: true, absent: true,
        before: { auth: { mode: "none" }, authSource: { scope: "default" }, effectiveRevision: "before" },
        after: { auth: { mode: "chap" }, authSource: { scope: "library" }, effectiveRevision: "after" },
      },
    ] });
    targetMock.listPublications.mockResolvedValue([{
      publicationId: "pub-online", libraryId: "lib-a", driveId: "drive-a", cartridgeId: "cart-a",
      targetIqn: "iqn.2026-01.example:holo-online", deviceRole: "drive", state: "ready",
    }, {
      publicationId: "pub-offline", libraryId: "lib-a", driveId: "drive-b", cartridgeId: "cart-b",
      targetIqn: "iqn.2026-01.example:holo-offline", deviceRole: "drive", state: "disabled",
    }]);
    targetMock.createPublication.mockResolvedValue({ publicationId: "pub-online-new", state: "ready" });

    renderWithProviders(<IscsiSecurityForm scope="library" ownerId="lib-a" title="Library policy" />);
    await openEditor();
    await chooseOption("Login check", "CHAP");
    await chooseOption("CHAP login credentials", "Backup host · backup");
    await userEvent.type(screen.getByLabelText(/Allowed backup host IQNs/), "iqn.1991-05.com.microsoft:backup-host");
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(await screen.findByText(/Saving briefly disconnects affected targets/)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Save and bring online" }));

    await waitFor(() => expect(targetMock.createPublication).toHaveBeenCalledTimes(1));
    expect(targetMock.unpublish).toHaveBeenCalledWith("pub-online");
    expect(securityMock.putLibraryBinding).toHaveBeenCalledWith("lib-a", expect.objectContaining({
      auth: { mode: "chap", credentialId: "chap-a", initiators: ["iqn.1991-05.com.microsoft:backup-host"], restrictInitiators: false },
    }));
    expect(targetMock.createPublication).toHaveBeenCalledWith(expect.objectContaining({
      targetIqn: "iqn.2026-01.example:holo-online",
      libraryId: "lib-a",
      driveId: "drive-a",
      cartridgeId: "cart-a",
      actor: "web-console",
    }));
    expect(targetMock.createPublication).not.toHaveBeenCalledWith(expect.objectContaining({ targetIqn: "iqn.2026-01.example:holo-offline" }));
    expect(targetMock.unpublish.mock.invocationCallOrder[0]).toBeLessThan(securityMock.putLibraryBinding.mock.invocationCallOrder[0]);
    expect(securityMock.putLibraryBinding.mock.invocationCallOrder[0]).toBeLessThan(targetMock.createPublication.mock.invocationCallOrder[0]);
  });

  it("reports targets that stay offline when republishing fails after saving", async () => {
    securityMock.listCredentials.mockResolvedValue([{ credentialId: "chap-a", label: "Backup host", username: "backup", version: 1, createdAt: "", secretPresent: true }]);
    securityMock.previewLibraryBinding.mockResolvedValue({ targets: [{
      targetIqn: "iqn.2026-01.example:holo-online", changed: true, busy: true, administrativeOffline: false, absent: false,
      before: { auth: { mode: "none" }, authSource: { scope: "default" }, effectiveRevision: "before" },
      after: { auth: { mode: "chap" }, authSource: { scope: "library" }, effectiveRevision: "after" },
    }] });
    targetMock.listPublications.mockResolvedValue([{
      publicationId: "pub-online", libraryId: "lib-a", driveId: "drive-a", cartridgeId: "cart-a",
      targetIqn: "iqn.2026-01.example:holo-online", deviceRole: "drive", state: "ready",
    }]);
    targetMock.createPublication.mockRejectedValue(new Error("runtime unavailable"));

    renderWithProviders(<IscsiSecurityForm scope="library" ownerId="lib-a" title="Library policy" />);
    await openEditor();
    await chooseOption("Login check", "CHAP");
    await chooseOption("CHAP login credentials", "Backup host · backup");
    await userEvent.type(screen.getByLabelText(/Allowed backup host IQNs/), "iqn.1991-05.com.microsoft:backup-host");
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    await userEvent.click(await screen.findByRole("button", { name: "Save and bring online" }));

    expect(await screen.findByText(/Settings were saved, but these targets could not be brought back online: iqn.2026-01.example:holo-online/)).toBeInTheDocument();
    expect(securityMock.putLibraryBinding).toHaveBeenCalled();
    expect(targetMock.createPublication).toHaveBeenCalledTimes(1);
  });

  it("saves a drive-level authentication override", async () => {
    securityMock.listCredentials.mockResolvedValue([{ credentialId: "chap-a", label: "CHAP A", username: "target", version: 1, createdAt: "", secretPresent: true }]);
    securityMock.getDriveBinding.mockResolvedValue({
      binding: { ...loadedBinding, scope: "drive", ownerId: "drive-a", libraryId: "lib-a", generation: 2, auth: { mode: "chap", credentialId: "chap-a", initiators: ["iqn.2026-01.example:host"] } },
      targets: [],
    });
    securityMock.previewDriveBinding.mockResolvedValue({ targets: [] });
    securityMock.putDriveBinding.mockResolvedValue({ binding: { ...loadedBinding, scope: "drive", ownerId: "drive-a", generation: 3 } });
    renderWithProviders(<IscsiSecurityForm scope="drive" ownerId="drive-a" title="Drive policy" />);
    await openEditor();
    await screen.findByRole("dialog", { name: "Drive policy" });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(securityMock.putDriveBinding).toHaveBeenCalledWith("drive-a", {
      generation: 3,
      auth: { mode: "chap", credentialId: "chap-a", initiators: ["iqn.2026-01.example:host"], restrictInitiators: false },
          actor: "web-console",
    }));
  });

  it("explains that CHAP requires the backup host IQN before calling the API", async () => {
    securityMock.listCredentials.mockResolvedValue([{ credentialId: "chap-a", label: "CHAP A", username: "target", version: 1, createdAt: "", secretPresent: true }]);
    renderWithProviders(<IscsiSecurityForm scope="library" ownerId="lib-a" title="Library policy" />);
    await openEditor();
    await screen.findByRole("dialog", { name: "Library policy" });
    await chooseOption("Login check", "CHAP");
    await chooseOption("CHAP login credentials", "CHAP A · target");
    await userEvent.click(screen.getByRole("button", { name: "Save" }));

    expect(await screen.findByText("Enter the backup host's Initiator IQN before saving protection settings.")).toBeInTheDocument();
    expect(securityMock.previewLibraryBinding).not.toHaveBeenCalled();
    expect(securityMock.putLibraryBinding).not.toHaveBeenCalled();
  });

  it("rejects an invalid backup host IQN before previewing a CHAP change", async () => {
    securityMock.listCredentials.mockResolvedValue([{ credentialId: "chap-a", label: "CHAP A", username: "target", version: 1, createdAt: "", secretPresent: true }]);
    renderWithProviders(<IscsiSecurityForm scope="library" ownerId="lib-a" title="Library policy" />);
    await openEditor();
    await screen.findByRole("dialog", { name: "Library policy" });
    await chooseOption("Login check", "CHAP");
    await chooseOption("CHAP login credentials", "CHAP A · target");
    await userEvent.type(screen.getByLabelText(/Allowed backup host IQNs \(one per line\)/), "not-an-iqn");
    await userEvent.click(screen.getByRole("button", { name: "Save" }));

    expect(await screen.findByText("Check the IQN format, for example iqn.1991-05.com.microsoft:backup-host.")).toBeInTheDocument();
    expect(securityMock.previewLibraryBinding).not.toHaveBeenCalled();
  });

  it("supports both CHAP modes with a mutual-capable profile and clears the selection when changing modes", async () => {
    securityMock.listCredentials.mockResolvedValue([
      { credentialId: "chap-one-way", label: "One way", username: "target", version: 1, createdAt: "", secretPresent: true },
      { credentialId: "chap-mutual", label: "Mutual capable", username: "target", mutualUsername: "initiator", version: 1, createdAt: "", secretPresent: true },
    ]);
    renderWithProviders(<IscsiSecurityForm scope="library" ownerId="lib-a" title="Library policy" editorSection="chap" open modalOnly />);
    await screen.findByRole("dialog", { name: "Configure CHAP" });
    await chooseOption("Login check", "One-way CHAP");
    await userEvent.click(screen.getByRole("button", { name: "CHAP login credentials" }));
    expect(screen.getByRole("option", { name: "One way · target" })).toBeInTheDocument();
    await userEvent.click(screen.getByRole("option", { name: "Mutual capable · target" }));
    expect(screen.getByRole("button", { name: "CHAP login credentials" })).toHaveTextContent("Mutual capable · target");

    await chooseOption("Login check", "Mutual CHAP");
    expect(screen.getByRole("button", { name: "CHAP login credentials" })).toHaveTextContent("None");
    await userEvent.click(screen.getByRole("button", { name: "CHAP login credentials" }));
    expect(screen.getByRole("option", { name: "Mutual capable · target" })).toBeInTheDocument();
    expect(screen.queryByRole("option", { name: "One way · target" })).not.toBeInTheDocument();
  });

  it("shows the Mutual CHAP client-verification caveat and limits credentials to mutual pairs", async () => {
    securityMock.listCredentials.mockResolvedValue([
      { credentialId: "chap-one-way", label: "One way", username: "target", version: 1, createdAt: "", secretPresent: true },
      { credentialId: "chap-mutual", label: "Mutual", username: "target", mutualUsername: "initiator", version: 1, createdAt: "", secretPresent: true },
    ]);
    renderWithProviders(<IscsiSecurityForm scope="library" ownerId="lib-a" title="Library policy" />);
    await openEditor();
    await screen.findByRole("dialog", { name: "Library policy" });
    await chooseOption("Login check", "Mutual CHAP");
    expect(await screen.findByText("Confirm that your backup software supports mutual CHAP.")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "CHAP login credentials" }));
    expect(screen.getByRole("option", { name: "Mutual · target" })).toBeInTheDocument();
    expect(screen.queryByRole("option", { name: "One way · target" })).not.toBeInTheDocument();
    expect(document.body.textContent).not.toContain("forwardSecret");
  });

  it("blocks an empty CHAP credential before making an API request", async () => {
    renderWithProviders(<IscsiSecurityForm scope="library" ownerId="lib-a" title="Library policy" />);
    await openEditor();
    await chooseOption("Login check", "CHAP");
    await userEvent.click(screen.getByRole("button", { name: "Save" }));

    expect(await screen.findByText("Select a CHAP credential before saving.")).toBeInTheDocument();
    expect(securityMock.previewLibraryBinding).not.toHaveBeenCalled();
    expect(securityMock.putLibraryBinding).not.toHaveBeenCalled();
  });

  it("cancel clears unsaved form edits by reloading the stored policy", async () => {
    renderWithProviders(<IscsiSecurityForm scope="library" ownerId="lib-a" title="Library policy" />);
    await openEditor();
    await screen.findByRole("dialog", { name: "Library policy" });
    await chooseOption("Login check", "Do not use CHAP");
    await userEvent.type(screen.getByLabelText(/Allowed backup host IQNs \(one per line\)/), "iqn.1991-05.com.microsoft:unsaved");
    await userEvent.click(screen.getByRole("button", { name: "Cancel" }));
    await waitFor(() => {
      expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    });
    expect(securityMock.putLibraryBinding).not.toHaveBeenCalled();
  });

  it("requires confirmation before restarting a busy target and warns before weakening protection", async () => {
    securityMock.previewLibraryBinding.mockResolvedValue({ targets: [{
      targetIqn: "iqn.2026-01.example:holo", changed: true, busy: true, administrativeOffline: false, absent: false,
      before: { auth: { mode: "chap", credentialId: "chap-a", initiators: ["iqn.2026-01.example:host"] }, authSource: { scope: "library" } },
      after: { auth: { mode: "none" }, authSource: { scope: "library" } },
    }] });
    renderWithProviders(<IscsiSecurityForm scope="library" ownerId="lib-a" title="Library policy" />);
    await openEditor();
    fireEvent.click(await screen.findByRole("button", { name: "Save" }));

    expect(await screen.findByText(/Saving briefly disconnects affected targets/)).toBeInTheDocument();
    expect(screen.getByText(/Saving briefly disconnects affected targets/)).toBeInTheDocument();
    expect(screen.getByText(/Protection will be reduced/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Save and bring online" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Cancel" })).toBeInTheDocument();
    expect(securityMock.putLibraryBinding).not.toHaveBeenCalled();
  });

  it("restarts a busy target as part of the confirmed policy save", async () => {
    const targetIqn = "iqn.2026-01.example:target-a";
    const targetBinding = { scope: "target" as const, ownerId: targetIqn, targetIqn, generation: 2, auth: null };
    securityMock.listCredentials.mockResolvedValue([{ credentialId: "chap-a", label: "CHAP A", username: "backup", version: 1, createdAt: "", secretPresent: true }]);
    securityMock.getTargetBinding.mockResolvedValue({
      binding: targetBinding,
      resolved: { auth: { mode: "none" }, authSource: { scope: "default" }, effectiveRevision: "before" },
    });
    securityMock.previewTargetBinding.mockResolvedValue({ targets: [{
        targetIqn, changed: true, busy: true, administrativeOffline: false, absent: false,
        before: { auth: { mode: "none" }, authSource: { scope: "default" } },
        after: { auth: { mode: "chap", credentialId: "chap-a", initiators: ["iqn.1991-05.com.microsoft:backup"] }, authSource: { scope: "target" } },
      }] });
    securityMock.putTargetBinding.mockResolvedValue({ binding: { ...targetBinding, generation: 3 } });
    targetMock.listPublications.mockResolvedValue([{
      publicationId: "pub-a", libraryId: "lib-a", driveId: "drive-a", cartridgeId: "cart-a", targetIqn, deviceRole: "drive", state: "ready",
    }]);
    targetMock.createPublication.mockResolvedValue({ publicationId: "pub-b", state: "ready" });

    renderWithProviders(<IscsiSecurityForm scope="target" ownerId={targetIqn} title="Target policy" open modalOnly />);
    await screen.findByRole("dialog", { name: "Target policy" });
    await chooseOption("Login check", "CHAP");
    await chooseOption("CHAP login credentials", "CHAP A · backup");
    await userEvent.type(screen.getByLabelText(/Allowed backup host IQNs \(one per line\)/), "iqn.1991-05.com.microsoft:backup");
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(await screen.findByText(/Saving briefly disconnects affected targets/)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Save and bring online" }));
    await waitFor(() => expect(securityMock.putTargetBinding).toHaveBeenCalledWith(targetIqn, expect.objectContaining({
      auth: { mode: "chap", credentialId: "chap-a", initiators: ["iqn.1991-05.com.microsoft:backup"], restrictInitiators: false },
        })));
    expect(targetMock.unpublish).toHaveBeenCalledWith("pub-a");
    expect(targetMock.createPublication).toHaveBeenCalledWith(expect.objectContaining({ targetIqn, libraryId: "lib-a", driveId: "drive-a", cartridgeId: "cart-a" }));
    expect(securityMock.previewTargetBinding).toHaveBeenCalledTimes(1);
    expect(targetMock.unpublish.mock.invocationCallOrder[0]).toBeLessThan(securityMock.putTargetBinding.mock.invocationCallOrder[0]);
    expect(securityMock.putTargetBinding.mock.invocationCallOrder[0]).toBeLessThan(targetMock.createPublication.mock.invocationCallOrder[0]);
  });
});
