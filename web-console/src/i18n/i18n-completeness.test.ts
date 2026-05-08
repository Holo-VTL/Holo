import { describe, expect, it } from "vitest";
import zhCN from "./locales/zh-CN.json";
import enUS from "./locales/en-US.json";

function flatten(input: Record<string, unknown>, prefix = ""): string[] {
  const keys: string[] = [];
  for (const [k, v] of Object.entries(input)) {
    const path = prefix ? `${prefix}.${k}` : k;
    if (v && typeof v === "object" && !Array.isArray(v)) {
      keys.push(...flatten(v as Record<string, unknown>, path));
    } else {
      keys.push(path);
    }
  }
  return keys;
}

describe("i18n completeness", () => {
  it("has mirrored keys in zh-CN and en-US", () => {
    const zh = flatten(zhCN as Record<string, unknown>).sort();
    const en = flatten(enUS as Record<string, unknown>).sort();
    expect(zh).toEqual(en);
  });
});
