// #597 grade table — the fixture is generated from store.BenchCatalogEntry
// (deployed GET /v1/bench/catalog shape, hk f0681e5); internal/ui's Go test
// pins it to the Go type so this suite never renders a synthetic shape.

import { readFileSync, readdirSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { fetchCatalog } from "./api";
import { GradesApp, type LoadCatalog } from "./GradesApp";
import type { CatalogResponse } from "./types";

const here = dirname(fileURLToPath(import.meta.url));
const fixture = JSON.parse(readFileSync(join(here, "fixtures", "catalog.response.json"), "utf8")) as CatalogResponse;

afterEach(cleanup);

function loader(body: CatalogResponse | Error, calls?: CatalogQueryRecorder["calls"]): LoadCatalog {
  return async (query) => {
    calls?.push(query);
    if (body instanceof Error) {
      throw body;
    }
    return body;
  };
}

type CatalogQueryRecorder = { calls: { pool?: string; includeRetired?: boolean }[] };

const byGrade = (pool: string | undefined) => ({
  ...fixture,
  catalog: pool ? fixture.catalog.filter((row) => row.pool === pool && row.gate !== "consult_only" && row.retired_at === null) : fixture.catalog.filter((row) => row.retired_at === null),
});

describe("grade table renders the real catalog shape", () => {
  it("groups by grade S+→C and shows profile·effort·model·pool·gate per row", async () => {
    render(<GradesApp load={loader(byGrade(undefined))} />);
    await waitFor(() => expect(document.querySelectorAll(".gr-group").length).toBeGreaterThan(0));
    const grades = [...document.querySelectorAll(".gr-group")].map((g) => g.getAttribute("data-grade"));
    expect(grades).toEqual(["S+", "S", "A+", "A", "B"]);
    const firstRow = document.querySelector('.gr-group[data-grade="S+"] tbody tr')!;
    expect(firstRow.textContent).toContain("codex-sol");
    expect(firstRow.textContent).toContain("max");
    expect(firstRow.textContent).toContain("gpt-5.6-sol");
    expect(firstRow.textContent).toContain("codex");
    expect(firstRow.textContent).toContain("67.0");
    // gate notation is the API's own field — consult_only carries its reason
    const fable = document.querySelector('tr[data-profile="fable"]')!;
    expect(fable.querySelector('[data-gate="consult_only"]')!.textContent).toContain("consult_only");
    expect(fable.textContent).toContain("운영자 명시 자문 전용");
    // estimate notation comes from the ESTIMATED_* annotation, nulls stay honest
    const kiro = document.querySelector('tr[data-profile="kiro-sol"]')!;
    expect(kiro.textContent).toContain("추정");
    expect(kiro.textContent).toContain("ESTIMATED_NO_PUBLIC_BENCH");
    expect(kiro.textContent).toContain("미측정");
    expect(kiro.textContent).toContain("출처 없음");
    // the profile-default row names itself, effort is never a blank cell
    expect(kiro.querySelector(".gr-effort")!.textContent).toBe("기본");
    // freshness marker from the BFF envelope
    expect(screen.getByTestId("catalog-status").textContent).toContain("2026-09-23 12:00 UTC 확인 자료");
  });

  it("an empty catalog is an explicit empty state, not a bare table", async () => {
    render(<GradesApp load={loader({ generated_at: fixture.generated_at, catalog: [] })} />);
    await waitFor(() => expect(document.querySelector(".gr-empty")).toBeTruthy());
    expect(document.querySelector(".gr-empty")!.textContent).toContain("카탈로그가 비어 있습니다");
    expect(document.querySelector(".gr-table")).toBeNull();
  });

  it("a failed first load is an explicit error, not an empty table", async () => {
    render(<GradesApp load={loader(new Error("request failed: 500"))} />);
    await waitFor(() => expect(document.querySelector('[role="alert"]')).toBeTruthy());
    expect(document.querySelector('[role="alert"]')!.textContent).toContain("급표를 불러오지 못했습니다");
    expect(document.querySelector(".gr-table")).toBeNull();
    expect(document.querySelector(".gr-empty")).toBeNull();
  });

  it("a malformed payload is an error, not a partial render", async () => {
    render(<GradesApp load={loader({ generated_at: fixture.generated_at } as unknown as CatalogResponse)} />);
    await waitFor(() => expect(document.querySelector('[role="alert"]')).toBeTruthy());
  });
});

describe("retired rows", () => {
  it("are hidden by default and appear only after the opt-in refetch", async () => {
    const calls: CatalogQueryRecorder["calls"] = [];
    const withRetired = { ...fixture, catalog: fixture.catalog };
    const load: LoadCatalog = async (query) => {
      calls.push(query);
      return query.includeRetired ? withRetired : byGrade(query.pool);
    };
    render(<GradesApp load={load} />);
    await waitFor(() => expect(document.querySelector('tr[data-profile="codex-sol"]')).toBeTruthy());
    expect(document.querySelector('tr[data-profile="kimi-k3"]')).toBeNull();
    expect(calls[0]).toEqual({ pool: "", includeRetired: false });
    fireEvent.click(screen.getByLabelText(/retired 행도 보기/));
    await waitFor(() => expect(document.querySelector('tr[data-profile="kimi-k3"]')).toBeTruthy());
    expect(calls[calls.length - 1]).toEqual({ pool: "", includeRetired: true });
    expect(document.querySelector('tr[data-profile="kimi-k3"]')!.textContent).toContain("retired");
  });
});

describe("pool filter", () => {
  it("passes the choice to the server as ?pool= — no client-side filtering", async () => {
    const calls: CatalogQueryRecorder["calls"] = [];
    const load: LoadCatalog = async (query) => {
      calls.push(query);
      return byGrade(query.pool);
    };
    render(<GradesApp load={load} />);
    await waitFor(() => expect(document.querySelector('tr[data-profile="codex-sol"]')).toBeTruthy());
    const select = screen.getByLabelText("pool filter") as HTMLSelectElement;
    // options come from the unfiltered response — every active pool stays
    // selectable; moonshot exists only on a retired row, so it is absent
    // until "retired 행도 보기" is on.
    expect([...select.options].map((o) => o.value)).toEqual(["", "claude", "clinepass", "codex", "devin", "grok", "kiro"]);
    fireEvent.change(select, { target: { value: "claude" } });
    await waitFor(() => expect(calls[calls.length - 1]).toEqual({ pool: "claude", includeRetired: false }));
    await waitFor(() => expect(document.querySelector('tr[data-profile="opus"]')).toBeTruthy());
    // the ladder view names its consult_only exclusion
    expect(screen.getByTestId("pool-note").textContent).toContain("consult_only");
    expect(document.querySelector('tr[data-profile="fable"]')).toBeNull();
    expect(document.querySelector('tr[data-profile="codex-sol"]')).toBeNull();
  });

  it("the real fetch builds the /v1 query contract verbatim", async () => {
    const spy = vi.fn(async () => new Response(JSON.stringify(fixture), { status: 200 }));
    vi.stubGlobal("fetch", spy);
    await fetchCatalog({ pool: "claude", includeRetired: true });
    expect(spy).toHaveBeenCalledWith("/ui/api/bench/catalog?pool=claude&include_retired=1");
    await fetchCatalog();
    expect(spy).toHaveBeenCalledWith("/ui/api/bench/catalog");
    vi.unstubAllGlobals();
  });

  it("a failed fetch for a changed filter is an error, never the previous query's rows", async () => {
    const load: LoadCatalog = async (query) => {
      if (query.pool === "claude") {
        throw new Error("pool fetch failed");
      }
      return byGrade(query.pool);
    };
    render(<GradesApp load={load} />);
    await waitFor(() => expect(document.querySelector('tr[data-profile="codex-sol"]')).toBeTruthy());
    fireEvent.change(screen.getByLabelText("pool filter"), { target: { value: "claude" } });
    await waitFor(() => expect(document.querySelector('[role="alert"]')).toBeTruthy());
    // the all-pool table must not linger under the claude selection — the
    // rows it would show were never fetched for this query.
    expect(document.querySelector(".gr-table")).toBeNull();
    expect(document.querySelector('tr[data-profile="codex-sol"]')).toBeNull();
    // switching back shows the rows bound to that query again
    fireEvent.change(screen.getByLabelText("pool filter"), { target: { value: "" } });
    await waitFor(() => expect(document.querySelector('tr[data-profile="codex-sol"]')).toBeTruthy());
  });

  it("a failed same-query refresh keeps the last good table under a warning", async () => {
    let fail = false;
    const load: LoadCatalog = async () => {
      if (fail) {
        throw new Error("refresh failed");
      }
      return byGrade(undefined);
    };
    render(<GradesApp load={load} />);
    await waitFor(() => expect(document.querySelector('tr[data-profile="codex-sol"]')).toBeTruthy());
    fail = true;
    fireEvent.click(screen.getByRole("button", { name: "새로고침" }));
    await waitFor(() => expect(screen.getByTestId("catalog-status").textContent).toContain("갱신 실패"));
    expect(document.querySelector('tr[data-profile="codex-sol"]')).toBeTruthy();
    expect(document.querySelector('[role="alert"]')).toBeNull();
  });
});

describe("read-only surface", () => {
  it("no write methods or forms anywhere under src/queue-proto/grades or its BFF handler", () => {
    const dir = join(here);
    const files = readdirSync(dir).filter((f) => /\.(ts|tsx)$/.test(f) && !f.endsWith(".test.tsx"));
    const sources = files.map((f) => `${f}\n${readFileSync(join(dir, f), "utf8")}`);
    const bff = readFileSync(resolve(here, "..", "..", "..", "..", "..", "internal", "ui", "catalog.go"), "utf8");
    for (const text of [...sources, `catalog.go\n${bff}`]) {
      expect(text).not.toMatch(/method:\s*["'`](POST|PUT|PATCH|DELETE)/);
      expect(text).not.toMatch(/<form|onSubmit|MethodPost|MethodPut|MethodPatch|MethodDelete/);
    }
  });
});
