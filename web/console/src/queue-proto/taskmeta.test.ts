import { describe, expect, it } from "vitest";
import { parseTaskDocMeta, stripTaskFrontMatter } from "./taskmeta";

describe("parseTaskDocMeta — hk-task/v1 front-matter read contract", () => {
  it("reads summary and display_title from a gated block", () => {
    const body = "---\nschema: hk-task/v1\nsummary: Plane 반출 정책을 정한다\ndisplay_title: 짧은 표시 제목\n---\n# 본문\n";
    expect(parseTaskDocMeta(body)).toEqual({ summary: "Plane 반출 정책을 정한다", displayTitle: "짧은 표시 제목" });
  });

  it("returns null without a front-matter fence", () => {
    expect(parseTaskDocMeta("# 제목\nschema: hk-task/v1\nsummary: x")).toBeNull();
  });

  it("returns null when the schema gate differs or is absent", () => {
    expect(parseTaskDocMeta("---\nschema: other/v9\nsummary: x\n---\n")).toBeNull();
    expect(parseTaskDocMeta("---\nsummary: x\ndisplay_title: y\n---\n")).toBeNull();
    // a fenced block that is not front-matter-shaped also does not opt in
    expect(parseTaskDocMeta("---\nnot yaml at all\n---\n")).toBeNull();
  });

  it("missing fields read as null, never invented", () => {
    expect(parseTaskDocMeta("---\nschema: hk-task/v1\n---\nbody")).toEqual({ summary: null, displayTitle: null });
    expect(parseTaskDocMeta("---\nschema: hk-task/v1\nsummary: only this\n---\n")).toEqual({
      summary: "only this",
      displayTitle: null,
    });
  });

  it("unclosed fence is not metadata", () => {
    expect(parseTaskDocMeta("---\nschema: hk-task/v1\nsummary: x\n# no closing fence")).toBeNull();
  });

  it("strips simple quotes; empty values are absent", () => {
    const body = '---\nschema: hk-task/v1\nsummary: "quoted value"\ndisplay_title: \'\'\n---\n';
    expect(parseTaskDocMeta(body)).toEqual({ summary: "quoted value", displayTitle: null });
  });

  it("tolerates CRLF and ignores non key: value lines", () => {
    const body = "---\r\nschema: hk-task/v1\r\n- a list line\r\nsummary: ok\r\n---\r\ntext";
    expect(parseTaskDocMeta(body)).toEqual({ summary: "ok", displayTitle: null });
  });

  it("values stay text — markup in fields is not interpreted", () => {
    const body = "---\nschema: hk-task/v1\nsummary: <b>not html</b> **not bold**\n---\n";
    const meta = parseTaskDocMeta(body);
    expect(meta?.summary).toBe("<b>not html</b> **not bold**");
  });

  it("does not read a fence that is not at byte 0", () => {
    expect(parseTaskDocMeta("\n---\nschema: hk-task/v1\nsummary: x\n---\n")).toBeNull();
  });

  it("YAML block-scalar indicators read as absent, never as the value", () => {
    const body = "---\nschema: hk-task/v1\nsummary: >\ndisplay_title: |+\n---\n";
    expect(parseTaskDocMeta(body)).toEqual({ summary: null, displayTitle: null });
  });
});

describe("stripTaskFrontMatter — what the renderer sees", () => {
  it("removes only a gated block; ungated bodies render untouched", () => {
    const gated = "---\nschema: hk-task/v1\nsummary: s\n---\n# 본문\n";
    expect(stripTaskFrontMatter(gated)).toBe("# 본문\n");
    const ungated = "---\nsummary: s\n---\n# 본문\n";
    expect(stripTaskFrontMatter(ungated)).toBe(ungated);
    expect(stripTaskFrontMatter("# no fence")).toBe("# no fence");
  });
});
