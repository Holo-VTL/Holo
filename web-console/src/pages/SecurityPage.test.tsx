import { screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { renderWithProviders } from "../test/renderWithProviders";
import { SecurityPage } from "./SecurityPage";

vi.mock("../services/api", () => ({
  api: {
    targets: {
      listPublications: vi.fn().mockResolvedValue([{ publicationId: "pub-a", targetIqn: "iqn.a", portal: "10.0.0.1:3260", state: "ready" }]),
      listAccessRules: vi.fn().mockResolvedValue([]),
      replaceAccessRules: vi.fn(),
      authorize: vi.fn(),
      rollbackAccess: vi.fn(),
    },
    resources: {
      listCartridges: vi.fn().mockResolvedValue([{ cartridgeId: "car-a", poolId: "pool-a", libraryId: "lib-a", barcode: "VTA000L06", capacityBytes: 1000 }]),
    },
    policy: {
      createAccessPolicy: vi.fn(),
      createRetentionPolicy: vi.fn(),
    },
  },
}));

describe("SecurityPage", () => {
  it("renders security policy forms", async () => {
    renderWithProviders(<SecurityPage />);
    expect(await screen.findByText("Security Policies")).toBeInTheDocument();
    expect(await screen.findByText("Create Access Policy")).toBeInTheDocument();
  });
});
