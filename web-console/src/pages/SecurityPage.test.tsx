import { cleanup, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { renderWithProviders } from "../test/renderWithProviders";
import { api } from "../services/api";
import { generateChapSecret, SecurityPage } from "./SecurityPage";

vi.mock("../services/api", () => ({
  api: {
    iscsiSecurity: {
      listCredentials: vi.fn().mockResolvedValue([]),
      createCredential: vi.fn(),
      deleteCredential: vi.fn(),
    },
  },
}));

describe("SecurityPage", () => {
  afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

  it("shows CHAP settings without the removed policy tab", async () => {
    renderWithProviders(<SecurityPage />);
    expect(await screen.findByRole("heading", { name: "iSCSI connection security" })).toBeInTheDocument();
    expect(await screen.findByText("CHAP login credentials")).toBeInTheDocument();
    expect(screen.queryByRole("tab", { name: "Access & data protection" })).not.toBeInTheDocument();
  });

  it("generates printable non-space secrets accepted by CHAP", () => {
    const secret = generateChapSecret();
    expect(secret).toHaveLength(15);
    expect(secret).toMatch(/^[!-~]{15}$/);
    expect(secret.toUpperCase()).not.toMatch(/^NULL/);
  });

  it("shows a generated secret and copies it on the appliance HTTP console", async () => {
    const secureContextDescriptor = Object.getOwnPropertyDescriptor(window, "isSecureContext");
    const execCommandDescriptor = Object.getOwnPropertyDescriptor(document, "execCommand");
    let copiedValue = "";
    Object.defineProperty(window, "isSecureContext", { configurable: true, value: false });
    Object.defineProperty(document, "execCommand", {
      configurable: true,
      value: vi.fn(() => {
        copiedValue = document.querySelector<HTMLTextAreaElement>('textarea[readonly]')?.value || "";
        return true;
      }),
    });

    try {
      renderWithProviders(<SecurityPage />);
      await userEvent.click(await screen.findByRole("button", { name: "Add CHAP credentials" }));
      await userEvent.click(screen.getByRole("button", { name: "Generate Secret" }));

      const secret = screen.getByLabelText("Target CHAP secret") as HTMLInputElement;
      expect(secret).toHaveAttribute("type", "text");
      expect(secret.value).toMatch(/^[!-~]{15}$/);
      await userEvent.click(screen.getByRole("button", { name: "Copy" }));

      expect(await screen.findByText("Copied")).toBeInTheDocument();
      expect(copiedValue).toBe(secret.value);
    } finally {
      if (secureContextDescriptor) Object.defineProperty(window, "isSecureContext", secureContextDescriptor);
      else Reflect.deleteProperty(window, "isSecureContext");
      if (execCommandDescriptor) Object.defineProperty(document, "execCommand", execCommandDescriptor);
      else Reflect.deleteProperty(document, "execCommand");
    }
  });

  it("clears submitted CHAP secrets and never shows them in saved credential metadata", async () => {
    renderWithProviders(<SecurityPage />);
    await screen.findByRole("heading", { name: "iSCSI connection security" });
    await userEvent.click(screen.getByRole("button", { name: "Add CHAP credentials" }));
    const canary = "SecretCanary123!";
    const field = (label: string) => screen.getByLabelText(label) as HTMLInputElement;
    await userEvent.type(field("Alias (optional)"), "Canary");
    await userEvent.type(field("Target CHAP username"), "backup-user");
    await userEvent.type(field("Target CHAP secret"), canary);
    const credentialForm = field("Target CHAP secret").closest("form");
    await userEvent.click(within(credentialForm as HTMLFormElement).getByRole("button", { name: "Save credential" }));
    await waitFor(() => expect(api.iscsiSecurity.createCredential).toHaveBeenCalledWith(expect.objectContaining({ forwardSecret: canary })));
    await waitFor(() => expect(screen.queryByLabelText("Target CHAP secret")).not.toBeInTheDocument());
    expect(document.body.textContent).not.toContain(canary);
  });

  it("uses the Target CHAP username as the admin note when it is left blank", async () => {
    renderWithProviders(<SecurityPage />);
    await screen.findByRole("heading", { name: "iSCSI connection security" });
    await userEvent.click(screen.getByRole("button", { name: "Add CHAP credentials" }));
    await userEvent.type(screen.getByLabelText("Target CHAP username"), "backup-host-01");
    await userEvent.type(screen.getByLabelText("Target CHAP secret"), "AValidSecret123!");
    await userEvent.click(screen.getByRole("button", { name: "Save credential" }));

    await waitFor(() => expect(api.iscsiSecurity.createCredential).toHaveBeenCalledWith(expect.objectContaining({
      label: "backup-host-01",
      forwardUsername: "backup-host-01",
    })));
  });

});
