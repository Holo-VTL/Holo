// @vitest-environment node
import { describe, expect, it } from "vitest";
import { createDevelopmentProxy } from "./developmentProxy";

describe("development proxy origin", () => {
  it("preserves the browser Host for same-origin API checks", () => {
    const proxy = createDevelopmentProxy("http://127.0.0.1");
    expect(proxy["/v1"]).toEqual({ target: "http://127.0.0.1", changeOrigin: false });
    expect(proxy["/healthz"]).toEqual({ target: "http://127.0.0.1", changeOrigin: false });
  });
});
