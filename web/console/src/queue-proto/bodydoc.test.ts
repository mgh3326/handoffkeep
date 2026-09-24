import { describe, expect, it } from "vitest";
import { parseBodyDoc, scanTitleDocs, titleDocKeys } from "./bodydoc";
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

describe("scanTitleDocs", () => {
  it("an explicit 본문 hk:doc <key> is the only body candidate shape", () => {
    for (const title of [
      "quota v2 — 본문 hk:doc task/2026-09-22/quota",
      "본문: hk:doc task/2026-09-22/quota",
      "본문은 hk:doc task/2026-09-22/quota 입니다",
      "본문 문서 hk:doc task/2026-09-22/quota",
      "본문 hk:doc `task/2026-09-22/quota`",
    ]) {
      expect(scanTitleDocs(title), title).toEqual({ body: ["task/2026-09-22/quota"], related: [], ids: [] });
    }
  });

  it("a plain hk:doc citation is related, never a body candidate — even when it is the only one", () => {
    expect(scanTitleDocs("fix per hk:doc advice/2026-09-24/x")).toEqual({ body: [], related: ["advice/2026-09-24/x"], ids: [] });
    // "본문" followed by other words before the reference is not a marker.
    expect(scanTitleDocs("본문 링크 hk:doc advice/2026-09-24/x")).toEqual({
      body: [],
      related: ["advice/2026-09-24/x"],
      ids: [],
    });
  });

  it("a numeric citation is a document ID — never a key, never a body candidate", () => {
    expect(scanTitleDocs("자문 Q(hk:doc 2299)")).toEqual({ body: [], related: [], ids: ["2299"] });
    expect(scanTitleDocs("본문 hk:doc 2299")).toEqual({ body: [], related: [], ids: ["2299"] });
  });

  it("body and plain citations coexist; a key consumed as body is not relisted", () => {
    expect(scanTitleDocs("본문 hk:doc a/x, 참고 hk:doc b/y 와 hk:doc a/x")).toEqual({
      body: ["a/x"],
      related: ["b/y"],
      ids: [],
    });
  });

  it("duplicates and unusable keys collapse or drop", () => {
    expect(scanTitleDocs("hk:doc a/x hk:doc a/x 본문 hk:doc a/x 본문 hk:doc a/x")).toEqual({ body: ["a/x"], related: [], ids: [] });
    expect(scanTitleDocs("본문 hk:doc ../x hk:doc a/b")).toEqual({ body: [], related: ["a/b"], ids: [] });
    expect(scanTitleDocs("본문 hk:doc a/b.").body).toEqual(["a/b"]);
  });

  it("a whitespace-padded title at the 64KiB cap scans in well under 50ms (no cubic backtracking)", () => {
    // S1 regression — the old BODY_MARKER_RE interleaved \s* between optional
    // single-char classes; "본문" + a long space run + no hk:doc blew up to
    // ~29s at n=4000. Titles can reach the 64KiB store cap, so scan a padded
    // near-miss at full size. This test takes minutes on the old regex.
    for (const title of ["본문" + " ".repeat(65_000), "본문 문서" + " ".repeat(65_000) + "hk:doc", "x".repeat(64 * 1024)]) {
      const t0 = performance.now();
      scanTitleDocs(title);
      expect(performance.now() - t0).toBeLessThan(50);
    }
  });

  it("several explicit body candidates stay several — order preserved, none picked", () => {
    expect(scanTitleDocs("본문 hk:doc a/x 그리고 본문 hk:doc b/y").body).toEqual(["a/x", "b/y"]);
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
