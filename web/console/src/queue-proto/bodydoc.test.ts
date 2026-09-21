import { describe, expect, it } from "vitest";
import { parseBodyDoc, titleDocKeys } from "./bodydoc";
import { findSectionHeading, safeDocUrl } from "./docrender";

describe("parseBodyDoc", () => {
  it("splits key and transitional section", () => {
    expect(parseBodyDoc("design/2026-09-21/x")).toEqual({ key: "design/2026-09-21/x", section: null });
    expect(parseBodyDoc("design/2026-09-21/x#3")).toEqual({ key: "design/2026-09-21/x", section: "3" });
    expect(parseBodyDoc("k#§3-태스크-가")).toEqual({ key: "k", section: "§3-태스크-가" });
  });

  it("refuses anything that is not key-shaped", () => {
    for (const bad of ["", "../x", "k/../x", "/abs", "a b", "본문", "k#", "k#a b", "k#a#b", "#only", "javascript:alert(1)", "k\n#x"]) {
      expect(parseBodyDoc(bad), bad).toBeNull();
    }
  });
});

describe("titleDocKeys", () => {
  it("finds hk:doc keys written into a title, deduplicated, trailing period dropped", () => {
    expect(titleDocKeys("fix it — 본문 hk:doc design/2026-09-21/a. 참고 hk:doc `design/b` and hk:doc design/2026-09-21/a")).toEqual([
      "design/2026-09-21/a",
      "design/b",
    ]);
  });

  it("returns nothing for a plain title or a traversal key", () => {
    expect(titleDocKeys("plain one-line title")).toEqual([]);
    expect(titleDocKeys("hk:doc ../etc/passwd")).toEqual([]);
  });
});

describe("safeDocUrl — the document link policy", () => {
  it("keeps https, same-origin paths and in-document fragments", () => {
    expect(safeDocUrl("https://github.com/x/y/pull/7")).toBe("https://github.com/x/y/pull/7");
    expect(safeDocUrl("/ui/tasks/5")).toBe("/ui/tasks/5");
    expect(safeDocUrl("#user-content-fn-1")).toBe("#user-content-fn-1");
    expect(safeDocUrl("docs/a?b=1#c")).toBe("/docs/a?b=1#c");
  });

  it("drops every other scheme and every cross-origin trick", () => {
    for (const bad of [
      "javascript:alert(1)",
      "JAVASCRIPT:alert(1)",
      " javascript:alert(1)",
      "java\tscript:alert(1)",
      "java\nscript:alert(1)",
      "data:text/html,<script>alert(1)</script>",
      "vbscript:msgbox(1)",
      "file:///etc/passwd",
      "http://example.com/",
      "mailto:a@example.com",
      "//evil.example/x",
      "\\\\evil.example/x",
      "/\\evil.example/x",
      "\\/evil.example/x",
      "",
    ]) {
      expect(safeDocUrl(bad), JSON.stringify(bad)).toBeUndefined();
    }
  });
});

describe("findSectionHeading", () => {
  it("prefers an exact normalized match, then a prefix match", () => {
    const root = document.createElement("div");
    root.innerHTML = "<h2>13. other</h2><h2>3. 태스크 가 — body</h2><h3>3</h3>";
    expect(findSectionHeading(root, "3")?.tagName).toBe("H3");
    expect(findSectionHeading(root, "3-태스크-가")?.textContent).toBe("3. 태스크 가 — body");
    expect(findSectionHeading(root, "%EC%97%86%EC%9D%8C")).toBeNull();
  });
});
