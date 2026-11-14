// #529 DeployPanel — the mutant-facing render rules: a record-less service
// reads "기록 없음" (never "최신"), the merged list heading is "마지막 배포
// 이후 머지됨" (never "미배포"), a null deployed_at measures the list from the
// record key and says so, and a failed/rolled_back latest attempt shows next
// to — never instead of — the last success.

import { readFileSync } from "node:fs";
import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import { DeploysPage } from "./DeploysPage";
import { deployElapsed, deployPRLabel, deployStamp, shortDeployedRef, type DeployPendingResponse } from "./deploy";

const SHA = "92ee9e2d0ddc47593677adcc9e83040dd9873463";
const DIGEST = "sha256:e0580273a4c31cd56a1028389212a1e101c40f130231f6dd2c49b3b5bc0214f6";

function fixture(over: Partial<DeployPendingResponse> = {}): DeployPendingResponse {
  return {
    generated_at: "2026-09-23T04:00:00Z",
    pr_source: "refs.pr",
    services: [
      {
        service: "handoffkeep",
        repo: "mgh3326/handoffkeep",
        target: "ncp",
        doc_url: "/ui/doc/deploy/handoffkeep/",
        record_count: 1,
        state: "pending",
        current: {
          record_key: "deploy/handoffkeep/20260923T033049Z",
          result: "success",
          target: "ncp",
          head_sha: SHA,
          deployed_ref: SHA,
          deployed_at: "2026-09-22T13:40:36Z",
          failed_step: null,
          failed_step_name: null,
          recorded_at: "2026-09-23T03:30:49Z",
          source: "backfill",
          included_prs: [
            "https://github.com/mgh3326/handoffkeep/pull/20",
            "https://github.com/mgh3326/handoffkeep/pull/21",
          ],
        },
        merged_boundary: "deployed_at",
        recent: [],
        merged_since: [
          {
            task_id: 447,
            title: "console decision fix",
            pr: "https://github.com/mgh3326/handoffkeep/pull/24",
            merged_at: "2026-09-23T01:00:00Z",
          },
        ],
      },
      {
        service: "auto_trader",
        repo: "mgh3326/auto_trader",
        target: "ncp",
        doc_url: "/ui/doc/deploy/auto_trader/",
        record_count: 1,
        state: "deployed",
        current: {
          record_key: "deploy/auto_trader/20260923T033052Z",
          result: "success",
          target: "ncp",
          head_sha: null,
          deployed_ref: DIGEST,
          deployed_at: null,
          failed_step: null,
          failed_step_name: null,
          recorded_at: "2026-09-23T03:30:52Z",
          source: "backfill",
          included_prs: [],
        },
        merged_boundary: "deploy_record_key",
        recent: [],
        merged_since: [],
      },
      {
        service: "panewire-hub",
        repo: "mgh3326/panewire",
        doc_url: "/ui/doc/deploy/panewire-hub/",
        record_count: 0,
        state: "no_record",
        current: null,
        merged_boundary: "no_current",
        recent: [],
        merged_since: [],
      },
    ],
    ...over,
  };
}

function fetcher(data: DeployPendingResponse) {
  return vi.fn(() => Promise.resolve(data));
}

describe("DeployPanel", () => {
  it("renders the current version line with short ref, deployed_at and result", async () => {
    render(<DeploysPage fetchStatus={fetcher(fixture())} pollMs={600_000} />);
    await waitFor(() => expect(screen.getByText("92ee9e2d0ddc")).toBeTruthy());
    expect(screen.getAllByText("현재 판").length).toBeGreaterThan(0);
    expect(screen.getAllByText("성공").length).toBeGreaterThan(0);
    expect(screen.getByText("14시간 전").getAttribute("title")).toBe("22/09, 22:40 KST");
  });

  it("renders a record-less service as 기록 없음 — never as 최신 (mutant: no fake current)", async () => {
    render(<DeploysPage fetchStatus={fetcher(fixture())} pollMs={600_000} />);
    await waitFor(() => expect(screen.getByText("기록 없음")).toBeTruthy());
    const pw = screen.getByLabelText("panewire-hub 배포");
    expect(pw.textContent).not.toContain("최신");
    expect(pw.textContent).not.toContain("pw-e401923");
  });

  it("keeps the honest heading — '마지막 배포 이후 머지됨', never '미배포' (mutant: heading)", async () => {
    render(<DeploysPage fetchStatus={fetcher(fixture())} pollMs={600_000} />);
    await waitFor(() => expect(screen.getAllByText(/마지막 배포 이후 머지됨/).length).toBeGreaterThan(0));
    expect(document.body.textContent).not.toContain("미배포");
  });

  it("renders the merged-since list with task and PR links", async () => {
    render(<DeploysPage fetchStatus={fetcher(fixture())} pollMs={600_000} />);
    await waitFor(() => expect(screen.getByText("console decision fix")).toBeTruthy());
    const task = screen.getByText("#447") as HTMLAnchorElement;
    expect(task.href).toContain("/ui/tasks/447");
    const pr = screen.getByText("#24") as HTMLAnchorElement;
    expect(pr.href).toBe("https://github.com/mgh3326/handoffkeep/pull/24");
  });

  it("shows the running SHA, environment and included PRs from the record", async () => {
    render(<DeploysPage fetchStatus={fetcher(fixture())} pollMs={600_000} />);
    const card = await screen.findByLabelText("handoffkeep 배포");
    // The running commit — the record's head_sha, not just the image ref.
    expect(within(card).getAllByText("92ee9e2d0ddc").length).toBeGreaterThan(0);
    expect(within(card).getByText("ncp")).toBeTruthy();
    expect(within(card).getByText("포함 PR")).toBeTruthy();
    const pr = within(card).getByText("#20") as HTMLAnchorElement;
    expect(pr.href).toBe("https://github.com/mgh3326/handoffkeep/pull/20");
  });

  it("names the state the records prove and never invents an unrecorded one", async () => {
    render(<DeploysPage fetchStatus={fetcher(fixture())} pollMs={600_000} />);
    // success + a merge waiting → pending; success alone → deployed.
    await waitFor(() => expect(screen.getByText("배포 대기")).toBeTruthy());
    expect(screen.getByText("배포됨")).toBeTruthy();
    // A record-less service already says "기록 없음"; it gets no second state chip.
    const pw = screen.getByLabelText("panewire-hub 배포");
    expect(pw.getAttribute("data-state")).toBe("no_record");
    expect(pw.querySelector('[data-chip="status"]')).toBeNull();
  });

  it("names an interrupted tick as serving-unknown, not as the old version serving", async () => {
    // An interrupted tick writes a failed step-6 record without knowing
    // which services flipped; the card must not claim a serving version.
    const data = fixture();
    data.services[1].state = "serving_unknown";
    render(<DeploysPage fetchStatus={fetcher(data)} pollMs={600_000} />);
    const at = await screen.findByLabelText("auto_trader 배포");
    expect(at.getAttribute("data-state")).toBe("serving_unknown");
    expect(within(at).getByText("서빙 판 미확인")).toBeTruthy();
    expect(at.querySelector('[data-chip="status"]')?.getAttribute("data-group")).toBe("failed");
  });

  it("shows SHA 미기록 when a record carries no head_sha, never a guess", async () => {
    render(<DeploysPage fetchStatus={fetcher(fixture())} pollMs={600_000} />);
    const at = await screen.findByLabelText("auto_trader 배포");
    expect(within(at).getAllByText("SHA 미기록").length).toBeGreaterThan(0);
  });

  it("a null deployed_at measures the merged list from the record key and says so", async () => {
    const data = fixture();
    const at = data.services[1];
    at.state = "pending";
    at.merged_since = [{ task_id: 77, title: "Merged after the record key", pr: "https://github.com/mgh3326/auto_trader/pull/7", merged_at: "2026-09-24T01:00:00Z" }];
    render(<DeploysPage fetchStatus={fetcher(data)} pollMs={600_000} />);
    const card = await screen.findByLabelText("auto_trader 배포");
    expect(within(card).getByText(/배포 기록 키의 시각을 기준으로 합니다/)).toBeTruthy();
    expect(within(card).getByText("마지막 배포 이후 머지됨 (1)")).toBeTruthy();
    expect(within(card).getByText("Merged after the record key")).toBeTruthy();
    // The list is real, so the old "cannot be computed" note is gone.
    expect(within(card).queryByText(/머지 목록을 계산할 수 없습니다/)).toBeNull();
  });

  it("shows a failed latest attempt next to the last success, with the serving caveat", async () => {
    const data = fixture();
    const hk = data.services[0];
    const latest = {
      record_key: "deploy/handoffkeep/20260923T050000Z",
      result: "failed",
      target: "ncp",
      head_sha: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
      deployed_ref: null,
      deployed_at: "2026-09-23T03:50:00Z",
      failed_step: 7,
      failed_step_name: "healthz_handoffkeep",
      recorded_at: "2026-09-23T03:50:00Z",
      source: "installer",
      included_prs: [],
      serving_maybe_changed: true,
    };
    hk.latest = latest;
    hk.recent = [latest];
    render(<DeploysPage fetchStatus={fetcher(data)} pollMs={600_000} />);
    await waitFor(() => expect(screen.getAllByText("최근 시도").length).toBeGreaterThan(0));
    expect(screen.getAllByText("실패").length).toBeGreaterThan(0);
    expect(screen.getByText("7단계")).toBeTruthy();
    expect(screen.getByText("healthz_handoffkeep")).toBeTruthy();
    expect(screen.getByText(/서빙 판이 바뀌었을 수 있음/)).toBeTruthy();
    // the success line is still there — the failed attempt never replaces it
    expect(screen.getByText("92ee9e2d0ddc")).toBeTruthy();
  });

  it("renders rolled_back as 롤백", async () => {
    const data = fixture();
    const latest = {
      record_key: "deploy/handoffkeep/20260923T050000Z",
      result: "rolled_back",
      target: "ncp",
      head_sha: "cccccccccccccccccccccccccccccccccccccccc",
      deployed_ref: null,
      deployed_at: "2026-09-23T03:50:00Z",
      failed_step: 7,
      failed_step_name: "healthz_handoffkeep",
      recorded_at: "2026-09-23T03:50:00Z",
      source: "installer",
      included_prs: [],
    };
    data.services[0].latest = latest;
    data.services[0].recent = [latest];
    render(<DeploysPage fetchStatus={fetcher(data)} pollMs={600_000} />);
    await waitFor(() => expect(screen.getAllByText("롤백").length).toBeGreaterThan(0));
  });

  it("a service with records but no success says so — not '기록 없음'", async () => {
    const data = fixture();
    const pw = data.services[2];
    pw.record_count = 1;
    pw.latest = {
      record_key: "deploy/panewire-hub/20260923T050000Z",
      result: "failed",
      target: "ncp",
      head_sha: null,
      deployed_ref: null,
      deployed_at: null,
      failed_step: 0,
      failed_step_name: null,
      recorded_at: "2026-09-23T03:50:00Z",
      source: "installer",
      included_prs: [],
    };
    render(<DeploysPage fetchStatus={fetcher(data)} pollMs={600_000} />);
    await waitFor(() => expect(screen.getByText("성공 배포 기록 없음")).toBeTruthy());
  });

  it("a first-load failure is an explicit error, not an empty panel", async () => {
    render(<DeploysPage fetchStatus={vi.fn(() => Promise.reject(new Error("boom")))} pollMs={600_000} />);
    await waitFor(() => expect(screen.getByText(/불러오지 못했습니다/)).toBeTruthy());
    expect(document.querySelector("[data-deploy-state]")?.getAttribute("data-deploy-state")).toBe("error");
  });

  it("a nested malformed payload fails explicitly instead of crashing render", async () => {
    // services:[null] passes a shallow Array.isArray check but must never
    // reach the row renderer — the panel shows the failure state.
    const malformed = { generated_at: "2026-09-23T04:00:00Z", pr_source: "refs.pr", services: [null] };
    render(
      <DeploysPage
        fetchStatus={vi.fn(() => Promise.resolve(malformed as unknown as DeployPendingResponse))}
        pollMs={600_000}
      />,
    );
    await waitFor(() => expect(screen.getByText(/불러오지 못했습니다/)).toBeTruthy());
    expect(document.querySelector("[data-deploy-state]")?.getAttribute("data-deploy-state")).toBe("error");
  });

  it("a resolved payload missing record fields is rejected, not half-rendered", async () => {
    const data = fixture() as unknown as Record<string, unknown>;
    (data.services as unknown[])[0] = { service: "handoffkeep" };
    render(
      <DeploysPage
        fetchStatus={vi.fn(() => Promise.resolve(data as unknown as DeployPendingResponse))}
        pollMs={600_000}
      />,
    );
    await waitFor(() => expect(screen.getByText(/불러오지 못했습니다/)).toBeTruthy());
  });

  it("a merged_since row that is not a task is rejected, not crashed on (r2 probe)", async () => {
    // merged_since:[null] passes Array.isArray but the row is dereferenced
    // during render — the payload must fail validation wholesale.
    const data = fixture();
    (data.services[0] as unknown as Record<string, unknown>).merged_since = [null];
    render(
      <DeploysPage
        fetchStatus={vi.fn(() => Promise.resolve(data as unknown as DeployPendingResponse))}
        pollMs={600_000}
      />,
    );
    await waitFor(() => expect(screen.getByText(/불러오지 못했습니다/)).toBeTruthy());
    expect(document.querySelector("[data-deploy-state]")?.getAttribute("data-deploy-state")).toBe("error");
  });

  it("a record missing nullable fields is rejected, not half-rendered (r2 probe)", async () => {
    // current:{record_key,result} satisfies a shallow record check but
    // renders "step undefined" — every field the panel reads is validated.
    const data = fixture();
    (data.services[0] as unknown as Record<string, unknown>).current = {
      record_key: "deploy/handoffkeep/20260923T050000Z",
      result: "success",
    };
    render(
      <DeploysPage
        fetchStatus={vi.fn(() => Promise.resolve(data as unknown as DeployPendingResponse))}
        pollMs={600_000}
      />,
    );
    await waitFor(() => expect(screen.getByText(/불러오지 못했습니다/)).toBeTruthy());
    expect(document.querySelector("[data-deploy-state]")?.getAttribute("data-deploy-state")).toBe("error");
  });

  it("a response missing required top-level fields is rejected (r3 probe)", async () => {
    // generated_at is the elapsed-time baseline — absent it the panel would
    // render "ready" with silently missing times instead of failing loudly.
    for (const field of ["generated_at", "pr_source"] as const) {
      const data = fixture() as unknown as Record<string, unknown>;
      delete data[field];
      const { unmount } = render(
        <DeploysPage
          fetchStatus={vi.fn(() => Promise.resolve(data as unknown as DeployPendingResponse))}
          pollMs={600_000}
        />,
      );
      await waitFor(() => expect(screen.getByText(/불러오지 못했습니다/)).toBeTruthy());
      unmount();
    }
  });

  it("a record result outside the contract enum is rejected (r4 probe)", async () => {
    const data = fixture();
    (data.services[0].current as unknown as Record<string, unknown>).result = "unknown";
    render(
      <DeploysPage
        fetchStatus={vi.fn(() => Promise.resolve(data as unknown as DeployPendingResponse))}
        pollMs={600_000}
      />,
    );
    await waitFor(() => expect(screen.getByText(/불러오지 못했습니다/)).toBeTruthy());
    expect(document.querySelector("[data-deploy-state]")?.getAttribute("data-deploy-state")).toBe("error");
  });

  it("a state outside the contract enum is rejected, not rendered", async () => {
    // check_failed is not a deploy-record state (holds write no record), so a
    // payload claiming it is malformed rather than silently accepted.
    const data = fixture();
    (data.services[0] as unknown as Record<string, unknown>).state = "check_failed";
    render(
      <DeploysPage
        fetchStatus={vi.fn(() => Promise.resolve(data as unknown as DeployPendingResponse))}
        pollMs={600_000}
      />,
    );
    await waitFor(() => expect(screen.getByText(/불러오지 못했습니다/)).toBeTruthy());
    expect(document.querySelector("[data-deploy-state]")?.getAttribute("data-deploy-state")).toBe("error");
  });

  it("a merged_boundary outside the contract enum is rejected", async () => {
    const data = fixture();
    (data.services[0] as unknown as Record<string, unknown>).merged_boundary = "guessed";
    render(
      <DeploysPage
        fetchStatus={vi.fn(() => Promise.resolve(data as unknown as DeployPendingResponse))}
        pollMs={600_000}
      />,
    );
    await waitFor(() => expect(screen.getByText(/불러오지 못했습니다/)).toBeTruthy());
  });

  it("under a capped scan, a shown current is scoped, not asserted as final (r5 probe)", async () => {
    // docs_capped means the scan stopped before the end of the key space —
    // an unscanned record could carry a later deployed_at, so the shown
    // current/latest are in-window results, never fleet truth.
    const data = fixture();
    const hk = data.services[0];
    hk.docs_capped = true;
    hk.recent = [hk.current!];
    hk.latest = {
      record_key: "deploy/handoffkeep/20260923T050000Z",
      result: "failed",
      target: "ncp",
      head_sha: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
      deployed_ref: null,
      deployed_at: "2026-09-23T03:50:00Z",
      failed_step: 7,
      failed_step_name: "healthz_handoffkeep",
      recorded_at: "2026-09-23T03:50:00Z",
      source: "installer",
      included_prs: [],
    };
    render(<DeploysPage fetchStatus={fetcher(data)} pollMs={600_000} />);
    await waitFor(() => expect(screen.getByText("현재 판(조회 범위 내)")).toBeTruthy());
    expect(screen.getByText("최근 시도(조회 범위 내)")).toBeTruthy();
    expect(screen.getByText(/현재 판·최근 시도·머지 목록 모두 조회 범위 안의 결과입니다/)).toBeTruthy();
  });

  it("invalid records list each key with its reason classes (#620 AC4)", async () => {
    // The operational incident: the key lost its seconds AND deployed_at was
    // not RFC3339 — one record carries both reasons.
    const data = fixture();
    data.services[0].invalid_count = 2;
    data.services[0].invalid = [
      { key: "deploy/handoffkeep/20260923T1456Z", reasons: ["key", "deployed_at"] },
      { key: "deploy/handoffkeep/20260924T010000Z", reasons: ["schema"] },
    ];
    render(<DeploysPage fetchStatus={fetcher(data)} pollMs={600_000} />);
    await waitFor(() => expect(screen.getByTestId("deploy-invalid-list")).toBeTruthy());
    const list = screen.getByTestId("deploy-invalid-list");
    expect(list.textContent).toContain("deploy/handoffkeep/20260923T1456Z");
    expect(list.textContent).toContain("키 시각 형식이 계약(YYYYMMDDTHHMMSSZ)이 아님");
    expect(list.textContent).toContain("deployed_at이 RFC3339가 아님");
    expect(list.textContent).toContain("deploy/handoffkeep/20260924T010000Z");
    expect(list.textContent).toContain("deploy-record/v0 본문이 아님(schema·service·result)");
  });

  it("unknown reason strings render verbatim so a newer server never hides why", async () => {
    const data = fixture();
    data.services[0].invalid_count = 1;
    data.services[0].invalid = [{ key: "deploy/handoffkeep/20260924T030000Z", reasons: ["doc_url_missing"] }];
    render(<DeploysPage fetchStatus={fetcher(data)} pollMs={600_000} />);
    await waitFor(() => expect(screen.getByText("doc_url_missing")).toBeTruthy());
  });

  it("count-only warning still renders when an older server sends no invalid list", async () => {
    const data = fixture();
    data.services[0].invalid_count = 3;
    render(<DeploysPage fetchStatus={fetcher(data)} pollMs={600_000} />);
    await waitFor(() =>
      expect(screen.getByText("형식이 맞지 않는 기록 3건은 표시하지 않았습니다.")).toBeTruthy()
    );
    expect(screen.queryByTestId("deploy-invalid-list")).toBeNull();
  });

  it("a malformed invalid row rejects the payload instead of half-rendering (r6 probe)", async () => {
    const data = fixture();
    data.services[0].invalid_count = 1;
    data.services[0].invalid = [{ key: "deploy/handoffkeep/20260923T1456Z", reasons: [] }];
    render(
      <DeploysPage
        fetchStatus={vi.fn(() => Promise.resolve(data as unknown as DeployPendingResponse))}
        pollMs={600_000}
      />,
    );
    await waitFor(() => expect(screen.getByText(/불러오지 못했습니다/)).toBeTruthy());
    expect(document.querySelector("[data-deploy-state]")?.getAttribute("data-deploy-state")).toBe("error");
  });

  it("under a capped scan, no in-window success reads 'unverified', not 'none'", async () => {
    // docs_capped + current:null means an older success may exist beyond the
    // scan window — the row must not assert "성공 배포 기록 없음".
    const data = fixture();
    const pw = data.services[2];
    pw.record_count = 200;
    pw.docs_capped = true;
    render(<DeploysPage fetchStatus={fetcher(data)} pollMs={600_000} />);
    await waitFor(() => expect(screen.getByText(/조회 범위 안에 성공 배포 기록이 없습니다/)).toBeTruthy());
    const section = screen.getByLabelText("panewire-hub 배포");
    expect(section.textContent).not.toContain("성공 배포 기록 없음");
    expect(section.textContent).toContain("더 오래된 배포 기록이 있을 수 있어");
  });
});


describe("deploy helpers", () => {
  it("shortens only real SHA/digest refs and never invents one", () => {
    expect(shortDeployedRef(SHA)).toBe("92ee9e2d0ddc");
    expect(shortDeployedRef(DIGEST)).toBe("sha256:e0580273a4c3…");
    expect(shortDeployedRef("pw-e401923")).toBe("pw-e401923");
    expect(shortDeployedRef(null)).toBeNull();
  });

  it("elapsed is measured from the server generated_at", () => {
    expect(deployElapsed("2026-09-23T04:00:00Z", "2026-09-22T13:40:36Z")).toBe("14시간 전");
    expect(deployElapsed("2026-09-23T04:00:00Z", null)).toBeNull();
    expect(deployElapsed("2026-09-23T04:00:00Z", "not-a-time")).toBeNull();
    expect(deployElapsed("2026-09-23T04:00:00Z", "2026-09-24T04:00:00Z")).toBeNull();
  });

  it("stamp and PR label degrade honestly", () => {
    expect(deployStamp(null)).toBeNull();
    expect(deployStamp("junk")).toBeNull();
    expect(deployPRLabel("https://github.com/mgh3326/handoffkeep/pull/24")).toBe("#24");
    expect(deployPRLabel("handoffkeep#24")).toBe("handoffkeep#24");
  });
});

describe("deploys.css", () => {
  it("has a rule for every dp- class the panel renders, and lays the included PRs out with a gap", async () => {
    render(<DeploysPage fetchStatus={fetcher(fixture())} pollMs={600_000} />);
    await screen.findByLabelText("handoffkeep 배포");
    const css = readFileSync("src/deploys/deploys.css", "utf8");
    const used = new Set<string>();
    for (const el of document.querySelectorAll('[class*="dp-"]')) {
      for (const name of Array.from(el.classList)) if (name.startsWith("dp-")) used.add(name);
    }
    expect(used.has("dp-prs")).toBe(true);
    const missing = [...used].filter((name) => !new RegExp(`\\.${name}(?![\\w-])`).test(css));
    expect(missing).toEqual([]);
    // Without the flex gap the label and links run together ("포함 PR#20#21").
    expect(css).toMatch(/\.dp-prs\s*\{[^}]*display:\s*flex[^}]*gap:/);
  });
});
