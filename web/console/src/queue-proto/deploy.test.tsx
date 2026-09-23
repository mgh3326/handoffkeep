// #529 DeployPanel — the mutant-facing render rules: a record-less service
// reads "기록 없음" (never "최신"), the merged list heading is "마지막 배포
// 이후 머지됨" (never "미배포"), a null deployed_at blocks the list with an
// explicit boundary note, and a failed/rolled_back latest attempt shows next
// to — never instead of — the last success.

import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { DeployPanel } from "./DeployPanel";
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
        current: {
          record_key: "deploy/handoffkeep/20260923T033049Z",
          result: "success",
          deployed_ref: SHA,
          deployed_at: "2026-09-22T13:40:36Z",
          failed_step: null,
          recorded_at: "2026-09-23T03:30:49Z",
          source: "backfill",
        },
        merged_boundary: "deployed_at",
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
        current: {
          record_key: "deploy/auto_trader/20260923T033052Z",
          result: "success",
          deployed_ref: DIGEST,
          deployed_at: null,
          failed_step: null,
          recorded_at: "2026-09-23T03:30:52Z",
          source: "backfill",
        },
        merged_boundary: "unrecorded",
        merged_since: [],
      },
      {
        service: "panewire-hub",
        repo: "mgh3326/panewire",
        doc_url: "/ui/doc/deploy/panewire-hub/",
        record_count: 0,
        current: null,
        merged_boundary: "no_current",
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
    render(<DeployPanel fetchStatus={fetcher(fixture())} pollMs={600_000} />);
    await waitFor(() => expect(screen.getByText("92ee9e2")).toBeTruthy());
    expect(screen.getAllByText("현재 판").length).toBeGreaterThan(0);
    expect(screen.getAllByText("success").length).toBeGreaterThan(0);
    expect(screen.getByText(/시간 전|일 전|분 전/)).toBeTruthy();
  });

  it("renders a record-less service as 기록 없음 — never as 최신 (mutant: no fake current)", async () => {
    render(<DeployPanel fetchStatus={fetcher(fixture())} pollMs={600_000} />);
    await waitFor(() => expect(screen.getByText("기록 없음")).toBeTruthy());
    const pw = screen.getByLabelText("panewire-hub 배포");
    expect(pw.textContent).not.toContain("최신");
    expect(pw.textContent).not.toContain("pw-e401923");
  });

  it("keeps the honest heading — '마지막 배포 이후 머지됨', never '미배포' (mutant: heading)", async () => {
    render(<DeployPanel fetchStatus={fetcher(fixture())} pollMs={600_000} />);
    await waitFor(() => expect(screen.getByText(/마지막 배포 이후 머지됨/)).toBeTruthy());
    expect(document.body.textContent).not.toContain("미배포");
  });

  it("renders the merged-since list with task and PR links", async () => {
    render(<DeployPanel fetchStatus={fetcher(fixture())} pollMs={600_000} />);
    await waitFor(() => expect(screen.getByText("console decision fix")).toBeTruthy());
    const task = screen.getByText("#447") as HTMLAnchorElement;
    expect(task.href).toContain("/ui/tasks/447");
    const pr = screen.getByText("#24") as HTMLAnchorElement;
    expect(pr.href).toBe("https://github.com/mgh3326/handoffkeep/pull/24");
  });

  it("a null deployed_at shows the boundary note and no list (mutant: no fabricated boundary)", async () => {
    render(<DeployPanel fetchStatus={fetcher(fixture())} pollMs={600_000} />);
    await waitFor(() => expect(screen.getByText(/배포 시각\(deployed_at\) 미기록/)).toBeTruthy());
  });

  it("shows a failed latest attempt next to the last success, with the serving caveat", async () => {
    const data = fixture();
    const hk = data.services[0];
    hk.latest = {
      record_key: "deploy/handoffkeep/20260923T050000Z",
      result: "failed",
      deployed_ref: null,
      deployed_at: "2026-09-23T03:50:00Z",
      failed_step: 7,
      recorded_at: "2026-09-23T03:50:00Z",
      source: "installer",
      serving_maybe_changed: true,
    };
    render(<DeployPanel fetchStatus={fetcher(data)} pollMs={600_000} />);
    await waitFor(() => expect(screen.getByText("최근 시도")).toBeTruthy());
    expect(screen.getByText("failed")).toBeTruthy();
    expect(screen.getByText("step 7")).toBeTruthy();
    expect(screen.getByText(/서빙 판이 바뀌었을 수 있음/)).toBeTruthy();
    // the success line is still there — the failed attempt never replaces it
    expect(screen.getByText("92ee9e2")).toBeTruthy();
  });

  it("renders rolled_back verbatim", async () => {
    const data = fixture();
    data.services[0].latest = {
      record_key: "deploy/handoffkeep/20260923T050000Z",
      result: "rolled_back",
      deployed_ref: null,
      deployed_at: "2026-09-23T03:50:00Z",
      failed_step: 5,
      recorded_at: "2026-09-23T03:50:00Z",
      source: "installer",
    };
    render(<DeployPanel fetchStatus={fetcher(data)} pollMs={600_000} />);
    await waitFor(() => expect(screen.getByText("rolled_back")).toBeTruthy());
  });

  it("a service with records but no success says so — not '기록 없음'", async () => {
    const data = fixture();
    const pw = data.services[2];
    pw.record_count = 1;
    pw.latest = {
      record_key: "deploy/panewire-hub/20260923T050000Z",
      result: "failed",
      deployed_ref: null,
      deployed_at: null,
      failed_step: 0,
      recorded_at: "2026-09-23T03:50:00Z",
      source: "installer",
    };
    render(<DeployPanel fetchStatus={fetcher(data)} pollMs={600_000} />);
    await waitFor(() => expect(screen.getByText("성공 배포 기록 없음")).toBeTruthy());
  });

  it("a first-load failure is an explicit error, not an empty panel", async () => {
    render(<DeployPanel fetchStatus={vi.fn(() => Promise.reject(new Error("boom")))} pollMs={600_000} />);
    await waitFor(() => expect(screen.getByText(/불러오지 못했습니다/)).toBeTruthy());
    expect(document.querySelector("[data-deploy-state]")?.getAttribute("data-deploy-state")).toBe("error");
  });

  it("a nested malformed payload fails explicitly instead of crashing render", async () => {
    // services:[null] passes a shallow Array.isArray check but must never
    // reach the row renderer — the panel shows the failure state.
    const malformed = { generated_at: "2026-09-23T04:00:00Z", pr_source: "refs.pr", services: [null] };
    render(
      <DeployPanel
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
      <DeployPanel
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
      <DeployPanel
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
      <DeployPanel
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
    render(<DeployPanel fetchStatus={fetcher(data)} pollMs={600_000} />);
    await waitFor(() => expect(screen.getByText(/조회 범위 안에 성공 배포 기록이 없습니다/)).toBeTruthy());
    const section = screen.getByLabelText("panewire-hub 배포");
    expect(section.textContent).not.toContain("성공 배포 기록 없음");
    expect(section.textContent).toContain("더 오래된 배포 기록이 있을 수 있습니다");
  });
});

describe("deploy helpers", () => {
  it("shortens only real SHA/digest refs and never invents one", () => {
    expect(shortDeployedRef(SHA)).toBe("92ee9e2");
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
