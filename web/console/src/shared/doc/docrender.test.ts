import { describe, expect, it } from "vitest";
import { safeDocUrl } from "./docrender";

describe("safeDocUrl", () => {
  it.each([
    ["#section", "#section"],
    ["/ui/doc/a/b?x=1#h", "/ui/doc/a/b?x=1#h"],
    ["https://example.com/a", "https://example.com/a"],
  ])("keeps %s", (raw, want) => {
    expect(safeDocUrl(raw)).toBe(want);
  });

  it.each(["javascript:alert(1)", "data:text/html,x", "http://example.com", "//example.com", "\\\\example.com", " ", "/a\u0000b", "https://"])(
    "drops %j",
    (raw) => {
      expect(safeDocUrl(raw)).toBeUndefined();
    },
  );
});
