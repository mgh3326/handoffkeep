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
  expect(screen.getByRole("button", { name: "검색 필터 해제" }).textContent).toBe("검색: compact list 해제");
});

it("has no separate 검색 input: free text is a quoted phrase in the query bar", async () => {
  render(<WorkPage />);
  await screen.findByText("Compact list");
  expect(screen.queryByLabelText("검색")).toBeNull();
  expect(screen.getByLabelText("검색 쿼리")).toBeTruthy();
  // The field selects stay.
  for (const name of ["레인", "종류", "상태", "등급", "티어", "정렬"]) expect(screen.getByLabelText(name)).toBeTruthy();
  fireEvent.change(screen.getByLabelText("검색 쿼리"), { target: { value: '"other work"' } });
  expect(screen.queryByText("Compact list")).toBeNull();
  expect(screen.getByText("Other work")).toBeTruthy();
});

it("shows only autocomplete while typing and the refusal on Enter or blur", async () => {
  render(<WorkPage />);
  await screen.findByText("Compact list");
  const query = screen.getByLabelText("검색 쿼리");
  fireEvent.focus(query);
  fireEvent.change(query, { target: { value: "상태:" } });
  expect(screen.queryByRole("alert")).toBeNull();
  expect(screen.getByRole("listbox", { name: "쿼리 자동완성" })).toBeTruthy();

  fireEvent.keyDown(query, { key: "Enter" });
  expect(screen.getByRole("alert").textContent).toContain("상태 값을 알 수 없습니다");

  fireEvent.change(query, { target: { value: "상태" } });
  fireEvent.change(query, { target: { value: "상태:" } });
  expect(screen.queryByRole("alert")).toBeNull();
  fireEvent.blur(query);
  expect(screen.getByRole("alert").textContent).toContain("상태 값을 알 수 없습니다");

  fireEvent.change(query, { target: { value: "상태:개발 중" } });
  expect(screen.queryByRole("alert")).toBeNull();
});

it("shows states by their Korean names in the select, the chip and the query", async () => {
  render(<WorkPage />);
  await screen.findByText("Compact list");
  const select = screen.getByLabelText("상태") as HTMLSelectElement;
  expect(Array.from(select.options).map(option => option.textContent)).toEqual(["전체", "개발 중", "머지됨", "검토 중"]);
  expect(screen.queryByText("developing")).toBeNull();

  fireEvent.change(select, { target: { value: "developing" } });
  expect(select.value).toBe("developing");
  expect(screen.getByRole("button", { name: "상태 필터 해제" }).textContent).toBe("상태: 개발 중 해제");
  expect((screen.getByLabelText("검색 쿼리") as HTMLInputElement).value).toContain("상태:개발 중");

  // An English value typed in the query is shown back in Korean once applied.
  const query = screen.getByLabelText("검색 쿼리");
  fireEvent.change(query, { target: { value: "state:reviewing" } });
  fireEvent.keyDown(query, { key: "Enter" });
  expect((screen.getByLabelText("검색 쿼리") as HTMLInputElement).value).toBe("상태:검토 중 프로젝트:a");
  expect(screen.getByRole("button", { name: "상태 필터 해제" }).textContent).toBe("상태: 검토 중 해제");
});

it("filters 우선순위:없음 to tasks without a priority", async () => {
  render(<WorkPage />);
  await screen.findByText("Compact list");
  fireEvent.change(screen.getByLabelText("검색 쿼리"), { target: { value: "우선순위:없음" } });
  expect(screen.queryByText("Compact list")).toBeNull();
  expect(screen.queryByText("Other work")).toBeNull();
  expect(screen.getByText("Done thing")).toBeTruthy();
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

  // Nothing saved yet: no heading, no empty-state text, no chip row.
  expect(screen.queryByRole("heading", { name: "저장한 검색" })).toBeNull();
  expect(screen.queryByText("저장한 검색이 없습니다.")).toBeNull();
  expect(screen.queryByRole("group", { name: "저장한 검색" })).toBeNull();

  // The save control sits in the query bar, not under the tabs.
  const saveButton = screen.getByRole("button", { name: "현재 검색 저장" });
  expect(saveButton.closest(".hk-query-row")).toBeTruthy();
  fireEvent.click(saveButton);
  fireEvent.change(screen.getByLabelText("저장 이름"), { target: { value: "내 검색" } });
  fireEvent.click(screen.getByRole("button", { name: "저장" }));
  expect(screen.getByRole("button", { name: "내 검색" })).toBeTruthy();
  expect(JSON.parse(values.get("hk-console:saved-searches") ?? "[]")).toHaveLength(1);
  // One compact chip row after the tabs, without a heading.
  const row = screen.getByRole("group", { name: "저장한 검색" });
  expect(row.querySelectorAll("ul")).toHaveLength(1);
  expect(screen.queryByRole("heading", { name: "저장한 검색" })).toBeNull();
  expect(screen.getByRole("navigation", { name: "작업 보기" }).compareDocumentPosition(row) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();

  fireEvent.change(screen.getByLabelText("상태"), { target: { value: "reviewing" } });
  expect((screen.getByLabelText("상태") as HTMLSelectElement).value).toBe("reviewing");
  fireEvent.click(screen.getByRole("button", { name: "내 검색" }));
  await waitFor(() => expect((screen.getByLabelText("상태") as HTMLSelectElement).value).toBe("developing"));

  expect(screen.queryByRole("button", { name: "이름" })).toBeNull();
  const renameButton = screen.getByRole("button", { name: "내 검색 이름 바꾸기" });
  expect(renameButton.textContent).toBe("이름 바꾸기");
  fireEvent.click(renameButton);
  fireEvent.change(screen.getByLabelText("내 검색 새 이름"), { target: { value: "새 검색" } });
  fireEvent.click(screen.getByRole("button", { name: "저장" }));
  expect(screen.getByRole("button", { name: "새 검색" })).toBeTruthy();
  expect(JSON.parse(values.get("hk-console:saved-searches") ?? "[]")[0].name).toBe("새 검색");

  fireEvent.click(screen.getByRole("button", { name: "새 검색 삭제" }));
  expect(screen.queryByRole("button", { name: "새 검색" })).toBeNull();
  expect(JSON.parse(values.get("hk-console:saved-searches") ?? "[]")).toHaveLength(0);
  expect(screen.queryByRole("group", { name: "저장한 검색" })).toBeNull();
});

it("refuses to save a query that is too long or a 51st search instead of cutting it", async () => {
  const values = new Map<string, string>();
  const existing = Array.from({ length: 50 }, (_, index) => ({ id: `id-${index}`, name: `검색 ${index}`, query: `상태:개발 중` }));
  values.set("hk-console:saved-searches", JSON.stringify(existing));
  vi.stubGlobal("localStorage", {
    getItem: (key: string) => values.get(key) ?? null,
    setItem: (key: string, value: string) => { values.set(key, value); },
    removeItem: (key: string) => { values.delete(key); },
  });
  render(<WorkPage />);
  await screen.findByText("Compact list");
  fireEvent.click(screen.getByRole("button", { name: "현재 검색 저장" }));
  expect(screen.getByRole("alert").textContent).toContain("50개까지");
  expect(screen.queryByLabelText("저장 이름")).toBeNull();
  expect(JSON.parse(values.get("hk-console:saved-searches") ?? "[]")).toHaveLength(50);
});
