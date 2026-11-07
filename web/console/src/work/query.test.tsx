import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { WorkPage } from "./WorkPage";

const base = { lane: "console-demo", kind: "build", priority: 2, created_by: "test", created_at: "2026-01-01T00:00:00Z", updated_at: "2026-01-01T00:00:00Z", project: "a" };
const tasks = [
  { ...base, id: 101, title: "Compact list", state: "developing", refs: { linear: { grade: "B", tier: "T2" } } },
  { ...base, id: 102, title: "Other work", state: "reviewing", lane: "fleet-status", kind: "feature", priority: 1, refs: { linear: { grade: "A", tier: "T1" } } },
  { ...base, id: 103, title: "Done thing", state: "merged", priority: 0, refs: {} },
];
const response = (value: unknown) => new Response(JSON.stringify(value), { status: 200 });

beforeEach(() => {
  vi.stubGlobal("matchMedia", () => ({ matches: false, addEventListener() {}, removeEventListener() {} }));
  vi.stubGlobal("fetch", vi.fn(async (path: string) => {
    if (path.includes("projects")) return response({ projects: ["a"] });
    if (path.includes("/inbox/state")) return response({ items: [] });
    return response({ tasks, states: ["developing", "reviewing", "merged"], generated_at: "2026-01-01T02:00:00Z", truncated: false });
  }));
  history.replaceState(null, "", "/ui/work?project=a&view=all");
});

afterEach(() => {
  vi.unstubAllGlobals();
  history.replaceState(null, "", "/");
});

it("round-trips the query text with the filter controls and the URL", async () => {
  render(<WorkPage />);
  await screen.findByText("Compact list");
  const query = screen.getByLabelText("검색 쿼리") as HTMLInputElement;
  expect(query.value).toBe("프로젝트:a");

  fireEvent.change(query, { target: { value: "상태:개발 중 우선순위:1" } });
  const params = new URLSearchParams(location.search);
  expect(params.get("state")).toBe("developing");
  expect(params.get("p")).toBe("1");
  expect(params.get("project")).toBe("a");
  expect((screen.getByLabelText("상태") as HTMLSelectElement).value).toBe("developing");
  expect((screen.getByLabelText("우선순위 ≥") as HTMLInputElement).value).toBe("1");

  fireEvent.change(screen.getByLabelText("상태"), { target: { value: "reviewing" } });
  expect((screen.getByLabelText("검색 쿼리") as HTMLInputElement).value).toContain("상태:검토 중");
  expect((screen.getByLabelText("검색 쿼리") as HTMLInputElement).value).toContain("우선순위:1");

  fireEvent.change(screen.getByLabelText("검색 쿼리"), { target: { value: "#진행중" } });
  expect(new URLSearchParams(location.search).get("view")).toBe("active");
  expect(screen.getByRole("button", { name: /진행 중 2/ }).getAttribute("aria-pressed")).toBe("true");

  fireEvent.change(screen.getByLabelText("검색 쿼리"), { target: { value: '"compact list"' } });
  expect(new URLSearchParams(location.search).get("q")).toBe("compact list");
  expect((screen.getByLabelText("검색") as HTMLInputElement).value).toBe("compact list");
});

it("toggles grade, tier and sort from the query and the controls", async () => {
  render(<WorkPage />);
  await screen.findByText("Compact list");
  fireEvent.change(screen.getByLabelText("검색 쿼리"), { target: { value: "등급:B 티어:T2 정렬:최근" } });
  expect((screen.getByLabelText("등급") as HTMLSelectElement).value).toBe("B");
  expect((screen.getByLabelText("티어") as HTMLSelectElement).value).toBe("T2");
  expect((screen.getByLabelText("정렬") as HTMLSelectElement).value).toBe("updated");
  expect(new URLSearchParams(location.search).get("sort")).toBe("updated");
  fireEvent.change(screen.getByLabelText("정렬"), { target: { value: "" } });
  expect((screen.getByLabelText("검색 쿼리") as HTMLInputElement).value).not.toContain("정렬:");
});

it("creates, applies, renames and deletes a saved search", async () => {
  const values = new Map<string, string>();
  vi.stubGlobal("localStorage", {
    getItem: (key: string) => values.get(key) ?? null,
    setItem: (key: string, value: string) => { values.set(key, value); },
    removeItem: (key: string) => { values.delete(key); },
  });
  render(<WorkPage />);
  await screen.findByText("Compact list");
  fireEvent.change(screen.getByLabelText("검색 쿼리"), { target: { value: "상태:개발 중" } });

  fireEvent.click(screen.getByRole("button", { name: "현재 검색 저장" }));
  fireEvent.change(screen.getByLabelText("저장 이름"), { target: { value: "내 검색" } });
  fireEvent.click(screen.getByRole("button", { name: "저장" }));
  expect(screen.getByRole("button", { name: "내 검색" })).toBeTruthy();
  expect(JSON.parse(values.get("hk-console:saved-searches") ?? "[]")).toHaveLength(1);

  fireEvent.change(screen.getByLabelText("상태"), { target: { value: "reviewing" } });
  expect((screen.getByLabelText("상태") as HTMLSelectElement).value).toBe("reviewing");
  fireEvent.click(screen.getByRole("button", { name: "내 검색" }));
  await waitFor(() => expect((screen.getByLabelText("상태") as HTMLSelectElement).value).toBe("developing"));

  fireEvent.click(screen.getByRole("button", { name: "내 검색 이름 변경" }));
  fireEvent.change(screen.getByLabelText("내 검색 새 이름"), { target: { value: "새 검색" } });
  fireEvent.click(screen.getByRole("button", { name: "저장" }));
  expect(screen.getByRole("button", { name: "새 검색" })).toBeTruthy();
  expect(JSON.parse(values.get("hk-console:saved-searches") ?? "[]")[0].name).toBe("새 검색");

  fireEvent.click(screen.getByRole("button", { name: "새 검색 삭제" }));
  expect(screen.queryByRole("button", { name: "새 검색" })).toBeNull();
  expect(JSON.parse(values.get("hk-console:saved-searches") ?? "[]")).toHaveLength(0);
});
