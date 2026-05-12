import { beforeEach, describe, expect, it, vi } from "vitest";
import { HOLO_API_KEY_SESSION_KEY, api, resetRuntimeConfigForTest } from "./api";

describe("api.storage", () => {
  beforeEach(() => {
    sessionStorage.clear();
    resetRuntimeConfigForTest();
    vi.unstubAllGlobals();
  });

  it("calls storage API without login headers", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(new Response(JSON.stringify({ apiBaseUrl: "" }), { status: 200 }))
      .mockResolvedValueOnce(
        new Response(JSON.stringify([]), {
          status: 200,
          headers: { "content-type": "application/json" },
        })
      );
    vi.stubGlobal("fetch", fetchMock);

    await api.storage.listPools();

    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(fetchMock.mock.calls[0][0]).toBe("/ui/config.json");
    const [url, init] = fetchMock.mock.calls[1] as [string, RequestInit];
    expect(url).toBe("/v1/storage/pools");
    expect((init.headers as Record<string, string>)["X-HOLO-API-Key"]).toBeUndefined();
  });

  it("sends configured API key for authenticated deployments", async () => {
    sessionStorage.setItem(HOLO_API_KEY_SESSION_KEY, "secret-key");
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(new Response(JSON.stringify({ apiBaseUrl: "" }), { status: 200 }))
      .mockResolvedValueOnce(
        new Response(JSON.stringify([]), {
          status: 200,
          headers: { "content-type": "application/json" },
        })
      );
    vi.stubGlobal("fetch", fetchMock);

    await api.storage.listPools();

    const [, init] = fetchMock.mock.calls[1] as [string, RequestInit];
    expect((init.headers as Record<string, string>)["X-HOLO-API-Key"]).toBe("secret-key");
  });

  it("uses runtime API base URL when configured", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(new Response(JSON.stringify({ apiBaseUrl: "http://10.10.1.184/" }), { status: 200 }))
      .mockResolvedValueOnce(
        new Response(JSON.stringify([]), {
          status: 200,
          headers: { "content-type": "application/json" },
        })
      );
    vi.stubGlobal("fetch", fetchMock);

    await api.storage.listPools();

    expect(fetchMock.mock.calls[1][0]).toBe("http://10.10.1.184/v1/storage/pools");
  });
});
