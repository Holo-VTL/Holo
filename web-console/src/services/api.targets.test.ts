import { beforeEach, describe, expect, it, vi } from "vitest";
import { api } from "./api";

describe("api.targets", () => {
  beforeEach(() => {
    sessionStorage.clear();
  });

  it("creates publication payload without poolId", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ publicationId: "p-1" }), {
        status: 202,
        headers: { "content-type": "application/json" },
      })
    );
    vi.stubGlobal("fetch", fetchMock);

    await api.targets.createPublication({
      libraryId: "lib-1",
      driveId: "drive-1",
      cartridgeId: "car-1",
      targetIqn: "iqn.2026-04.ai.holo:test",
      actor: "tester",
    });

    const [_url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    const payload = JSON.parse(String(init.body));
    expect(payload.poolId).toBeUndefined();
    expect(payload.libraryId).toBe("lib-1");
  });
});
