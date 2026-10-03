import { beforeEach, describe, expect, it, vi } from "vitest";
import { api, resetRuntimeConfigForTest } from "./api";

describe("iSCSI security API client", () => {
  const fetchMock = vi.fn();

  beforeEach(() => {
    vi.clearAllMocks();
    resetRuntimeConfigForTest();
    fetchMock.mockImplementation(async (input: RequestInfo | URL) => {
      if (String(input).endsWith("/config.json")) {
        return new Response(JSON.stringify({ apiBaseUrl: "" }), { status: 200, headers: { "content-type": "application/json" } });
      }
      return new Response(JSON.stringify({ binding: { scope: "library", ownerId: "lib one", generation: 1 } }), {
        status: 200,
        headers: { "content-type": "application/json" },
      });
    });
    vi.stubGlobal("fetch", fetchMock);
  });

  it("sends the authentication policy to the encoded owner endpoint", async () => {
    await api.iscsiSecurity.putLibraryBinding("lib one", {
      generation: 1,
      auth: { mode: "chap", credentialId: "chap-a", initiators: ["iqn.2026-01.example:host"] },
    });
    const request = fetchMock.mock.calls[1];
    expect(request[0]).toBe("/v1/libraries/lib%20one/iscsi-security");
    expect(request[1]).toMatchObject({ method: "PUT" });
    expect(JSON.parse((request[1] as RequestInit).body as string)).toEqual({
      generation: 1,
      auth: { mode: "chap", credentialId: "chap-a", initiators: ["iqn.2026-01.example:host"] },
    });
  });
});
