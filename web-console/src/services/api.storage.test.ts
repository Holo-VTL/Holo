import { beforeEach, describe, expect, it, vi } from "vitest";
import { HOLO_API_KEY_SESSION_KEY, api } from "./api";

describe("api.storage", () => {
  beforeEach(() => {
    sessionStorage.clear();
  });

  it("calls storage API without login headers", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(JSON.stringify([]), {
        status: 200,
        headers: { "content-type": "application/json" },
      })
    );
    vi.stubGlobal("fetch", fetchMock);

    await api.storage.listPools();

    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("/v1/storage/pools");
    expect((init.headers as Record<string, string>)["X-HOLO-API-Key"]).toBeUndefined();
  });

  it("sends configured API key for authenticated deployments", async () => {
    sessionStorage.setItem(HOLO_API_KEY_SESSION_KEY, "secret-key");
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(JSON.stringify([]), {
        status: 200,
        headers: { "content-type": "application/json" },
      })
    );
    vi.stubGlobal("fetch", fetchMock);

    await api.storage.listPools();

    const [, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect((init.headers as Record<string, string>)["X-HOLO-API-Key"]).toBe("secret-key");
  });
});
