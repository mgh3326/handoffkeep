import { describe, expect, it } from "vitest";
import { formatQuery, parseQuery, querySuggestions, sortTasks, taskMatchesQuery, EMPTY_QUERY, type QueryFilters } from "./query";
import type { BoardTask } from "../boardTypes";
import { readSavedSearches, writeSavedSearches, makeSavedSearch, renameSavedSearch } from "../savedSearches";

const task = (over: Partial<BoardTask> = {}): BoardTask => ({
  id: 101, title: "Compact list", lane: "console-demo", kind: "build", state: "developing", priority: 2,
  created_by: "test", created_at: "2026-01-01T00:00:00Z", updated_at: "2026-01-02T00:00:00Z", refs: {}, project: "console-demo",
  ...over,
});

describe("query syntax", () => {
  it("parses every field in Korean and English", () => {
    const parsed = parseQuery("상태:검토 중 레인:console-demo 프로젝트:console-demo 종류:build 우선순위:1 등급:B 티어:T2 정렬:최근");
    expect(parsed.errors).toEqual([]);
    expect(parsed.filters).toMatchObject({ state: "reviewing", lane: "console-demo", project: "console-demo", kind: "build", priority: "1", grade: "B", tier: "T2", sort: "updated" });
    const english = parseQuery("state:developing lane:fleet-status priority:P3 sort:priority");
    expect(english.errors).toEqual([]);
    expect(english.filters).toMatchObject({ state: "developing", lane: "fleet-status", priority: "3", sort: "priority" });
  });

  it("round-trips through formatQuery", () => {
    const filters: QueryFilters = { ...EMPTY_QUERY, text: "compact list", state: "developing", lane: "console-demo", project: "console-demo", kind: "build", priority: "1", grade: "B", tier: "T2", sort: "updated", view: "active" };
    const text = formatQuery(filters);
    expect(text).toBe("#진행중 \"compact list\" 상태:개발 중 레인:console-demo 프로젝트:console-demo 종류:build 우선순위:1 등급:B 티어:T2 정렬:최근");
    expect(parseQuery(text).filters).toEqual(filters);
  });

  it("matches 우선순위:없음 exactly and 1-4 as at least that urgent", () => {
    const none = parseQuery("우선순위:없음").filters;
    expect(taskMatchesQuery(task({ priority: 0 }), none)).toBe(true);
    expect(taskMatchesQuery(task({ priority: 2 }), none)).toBe(false);
    const two = parseQuery("우선순위:2").filters;
    expect(taskMatchesQuery(task({ priority: 2 }), two)).toBe(true);
    // The control's meaning is "priority number at least 2": P1 is hidden.
    expect(taskMatchesQuery(task({ priority: 1 }), two)).toBe(false);
  });

  it("maps the #미해결 and #진행중 shortcuts and keeps #101 as text", () => {
    expect(parseQuery("#미해결").filters.view).toBe("needs-me");
    expect(parseQuery("#진행중").filters.view).toBe("active");
    expect(parseQuery("#101").filters.text).toBe("#101");
    expect(parseQuery("#없는단축어").errors[0]).toContain("알 수 없는 단축어");
  });

  it("keeps a quoted phrase together and reports unknown fields and values", () => {
    expect(parseQuery('"compact list"').filters.text).toBe("compact list");
    expect(parseQuery('"a:b"').filters.text).toBe("a:b");
    expect(parseQuery(formatQuery({ ...EMPTY_QUERY, text: "a:b" })).filters.text).toBe("a:b");
    expect(parseQuery("foo:bar").errors[0]).toContain("알 수 없는 필터");
    expect(parseQuery("상태:없는상태").errors[0]).toContain("상태 값을 알 수 없습니다");
    expect(parseQuery("우선순위:9").errors[0]).toContain("1-4");
    expect(parseQuery("정렬:글쎄").errors[0]).toContain("정렬 값을 알 수 없습니다");
    expect(parseQuery("상태:개발 중 상태:검토 중").errors[0]).toContain("두 번");
  });

  it("filters tasks by every field", () => {
    const filters: QueryFilters = { ...EMPTY_QUERY, text: "compact", state: "developing", lane: "console-demo", project: "console-demo", kind: "build", priority: "2", grade: "B", tier: "T2" };
    expect(taskMatchesQuery(task({ refs: { linear: { grade: "B", tier: "T2" } } }), filters)).toBe(true);
    expect(taskMatchesQuery(task(), filters)).toBe(false);
    expect(taskMatchesQuery(task({ title: "Other" }), { ...filters, text: "compact" })).toBe(false);
    expect(taskMatchesQuery(task(), { ...filters, state: "reviewing" })).toBe(false);
    expect(taskMatchesQuery(task({ priority: 1 }), { ...filters, priority: "3" })).toBe(false);
  });

  it("sorts by priority, update time, id and state", () => {
    const tasks = [task({ id: 1, priority: 3, updated_at: "2026-01-01T00:00:00Z", state: "backlog" }), task({ id: 2, priority: 1, updated_at: "2026-01-03T00:00:00Z", state: "developing" }), task({ id: 3, priority: 0, updated_at: "2026-01-02T00:00:00Z", state: "merged" })];
    expect(sortTasks(tasks, "priority").map(item => item.id)).toEqual([2, 1, 3]);
    expect(sortTasks(tasks, "updated").map(item => item.id)).toEqual([2, 3, 1]);
    expect(sortTasks(tasks, "id").map(item => item.id)).toEqual([3, 2, 1]);
    expect(sortTasks(tasks, "state").map(item => item.id)).toEqual([1, 2, 3]);
  });

  it("suggests fields, state labels and the board's lanes", () => {
    const context = { lanes: ["console-demo"], projects: ["console-demo"], kinds: ["build"], grades: ["B"], tiers: ["T2"] };
    expect(querySuggestions("", context).items.map(item => item.value)).toContain("상태:");
    expect(querySuggestions("상태:", context).items.map(item => item.value)).toContain("개발 중");
    expect(querySuggestions("상태: 개", context).items.map(item => item.value)).toContain("개발 중");
    expect(querySuggestions("레인:console", context).items.map(item => item.value)).toEqual(["console-demo"]);
    expect(querySuggestions("등급:", context).items.map(item => item.value)).toEqual(["B"]);
    expect(querySuggestions("#", context).items.map(item => item.value)).toEqual(["#미해결", "#진행중"]);
  });
});

describe("saved searches", () => {
  it("drops malformed stored entries and keeps valid ones", () => {
    const values = new Map<string, string>();
    const storage = { getItem: (key: string) => values.get(key) ?? null, setItem: (key: string, value: string) => { values.set(key, value); } };
    const stored = [makeSavedSearch("내 작업", "상태:개발 중")!, { id: "", name: "bad", query: "x" }, { id: "2", name: "", query: "x" }, "nope"];
    values.set("hk-console:saved-searches", JSON.stringify(stored));
    const read = readSavedSearches(storage);
    expect(read).toHaveLength(1);
    expect(read[0].name).toBe("내 작업");
    writeSavedSearches(read, storage);
    expect(JSON.parse(values.get("hk-console:saved-searches") ?? "[]")).toHaveLength(1);
  });

  it("refuses an empty name or query and renames without changing the query", () => {
    expect(makeSavedSearch("  ", "상태:x")).toBeNull();
    expect(makeSavedSearch("name", "  ")).toBeNull();
    expect(makeSavedSearch("name", `"${"가".repeat(500)}"`)).toBeNull();
    const search = makeSavedSearch("name", "상태:개발 중")!;
    expect(renameSavedSearch(search, "  ")).toBeNull();
    expect(renameSavedSearch(search, "새 이름")).toEqual({ ...search, name: "새 이름" });
  });
});
