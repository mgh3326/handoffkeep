import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { PolicyPanel } from "./PolicyPanel";
import type { PolicyResponse } from "./types";

function policy(partial: Partial<PolicyResponse> = {}): PolicyResponse {
  return {
    status: "ok",
    pointer_key: "policy/active",
    pointer_doc_url: "/ui/doc/policy/active",
    manifest_key: "policy/releases/2026-09",
    manifest_doc_url: "/ui/doc/policy/releases/2026-09",
    release: "2026-09",
    items: [
      { key: "policy/rules/a", doc_url: "/ui/doc/policy/rules/a", exists: true, title: "Rule A" },
      { key: "policy/rules/b", doc_url: "/ui/doc/policy/rules/b", exists: false, title: "Rule B" },
    ],
    ...partial,
  };
}

describe("PolicyPanel", () => {
  it("links the exact pointer and manifest keys plus every manifest item", () => {
    render(<PolicyPanel policy={policy()} />);
    const pointer = screen.getByText("policy/active");
    expect(pointer.getAttribute("href")).toBe("/ui/doc/policy/active");
    const manifest = screen.getByText("policy/releases/2026-09");
    expect(manifest.getAttribute("href")).toBe("/ui/doc/policy/releases/2026-09");
    expect(screen.getByText(/release 2026-09/)).toBeTruthy();

    const links = screen.getAllByRole("link").map((link) => link.getAttribute("href"));
    expect(links).toContain("/ui/doc/policy/rules/a");
    expect(links).toContain("/ui/doc/policy/rules/b");
    for (const link of screen.getAllByRole("link")) {
      expect(link.getAttribute("target")).toBeNull();
    }
    expect(screen.getByText("문서 없음")).toBeTruthy();
  });

  it("renders item keys even when a doc title exists", () => {
    render(<PolicyPanel policy={policy()} />);
    expect(screen.getByText("Rule A")).toBeTruthy();
    expect(screen.getByText("policy/rules/a")).toBeTruthy();
  });

  it("marks truncated manifests without implying complete coverage", () => {
    render(<PolicyPanel policy={policy({ truncated: true })} />);
    expect(screen.getByText("일부만 표시됨")).toBeTruthy();
    expect(screen.getByText("Rule A")).toBeTruthy();
  });

  it("renders a loading placeholder while policy is unset", () => {
    render(<PolicyPanel policy={null} />);
    expect(screen.getByText("불러오는 중")).toBeTruthy();
  });

  it.each([
    ["not_configured", "활성 정책 릴리스가 설정되지 않았습니다."],
    ["invalid_pointer", "policy/active 포인터가 올바른 문서 키를 가리키지 않습니다."],
    ["manifest_missing", "포인터가 가리키는 매니페스트 문서가 없습니다."],
    ["invalid_manifest", "매니페스트 문서 형식이 올바르지 않습니다."],
  ] as const)("renders the %s status explicitly", (status, message) => {
    render(<PolicyPanel policy={{ status, pointer_key: "policy/active", pointer_doc_url: "/ui/doc/policy/active", items: [] }} />);
    expect(screen.getByText(message)).toBeTruthy();
    expect(screen.queryByRole("list")).toBeNull();
  });
});
