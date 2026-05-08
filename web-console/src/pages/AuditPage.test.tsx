import { screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { renderWithProviders } from "../test/renderWithProviders";
import { AuditPage } from "./AuditPage";

vi.mock("../services/api", () => ({
  api: {
    ops: {
      auditEvents: vi.fn().mockResolvedValue([
        { eventId: "evt-a", actor: "tester", action: "publish", objectType: "publication", objectId: "pub-a", result: "success", occurredAt: new Date().toISOString() },
      ]),
    },
    targets: {
      visible: vi.fn().mockResolvedValue([]),
      discovery: vi.fn().mockResolvedValue([]),
    },
  },
}));

describe("AuditPage", () => {
  it("renders audit events", async () => {
    renderWithProviders(<AuditPage />);
    expect(await screen.findByText("Audit & Discovery")).toBeInTheDocument();
    expect(await screen.findByText("publish")).toBeInTheDocument();
  });
});
