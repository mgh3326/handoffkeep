import { readFileSync } from "node:fs";
import { render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { FleetStatusPage } from "./FleetStatusPage";
import { goldenSnapshot } from "./fixture";
import type { FleetStatusResponse, FleetStatusSnapshot } from "./types";

function serve(body: unknown, status = 200) {
  vi.stubGlobal("fetch", vi.fn(async () => new Response(JSON.stringify(body), { status })));
}

function withSnapshot(snapshot: FleetStatusSnapshot | null): FleetStatusResponse {
  return { snapshot, updated_at: snapshot ? "2026-11-15T12:00:30Z" : undefined };
}

const css = () => readFileSync("src/fleet-status/fleetStatus.css", "utf8");

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("FleetStatusPage", () => {
  it("renders the three parts from the golden snapshot", async () => {
    serve(withSnapshot(goldenSnapshot));
    render(<FleetStatusPage />);

    const hosts = await screen.findByRole("region", { name: "호스트" });
    expect(within(hosts).getByText("node-a")).toBeTruthy();
    expect(within(hosts).getByText("node-b")).toBeTruthy();
    expect(within(hosts).getByText("연결됨")).toBeTruthy();
    expect(within(hosts).getByText("연결 끊김")).toBeTruthy();
    expect(within(hosts).getByText("스폰 수락")).toBeTruthy();
    expect(within(hosts).getByText("스폰 안 받음")).toBeTruthy();
    expect(hosts.textContent).toContain("레인");

    const quota = screen.getByRole("region", { name: "호스트별 쿼터" });
    expect(within(quota).getByText("claude")).toBeTruthy();
    expect(within(quota).getByText("claude:0123456789abcdef")).toBeTruthy();
    expect(within(quota).getByText("5h")).toBeTruthy();
    expect(within(quota).getByText("41%")).toBeTruthy();
    expect(within(quota).getByText("7d")).toBeTruthy();
    expect(within(quota).getByText("12%")).toBeTruthy();
    expect(within(quota).getByText("최신")).toBeTruthy();
    expect(within(quota).getByText(/측정/)).toBeTruthy();
    expect(within(quota).getByText(/경과/)).toBeTruthy();

    const plan = screen.getByRole("region", { name: "지금 배정된다면" });
    expect(within(plan).getByText("C")).toBeTruthy();
    expect(within(plan).getByText("배정됨")).toBeTruthy();
    expect(within(plan).getByText(/omp-dsflash41/)).toBeTruthy();
    expect(within(plan).getByText(/commandcode/)).toBeTruthy();
    expect(within(plan).getByText(/node-a/)).toBeTruthy();
    expect(within(plan).getByText(/ranked/)).toBeTruthy();
    expect(within(plan).getByText("A")).toBeTruthy();
    expect(within(plan).getByText("가능한 호스트 없음")).toBeTruthy();
  });

  it("marks a stale quota reading and carries the plan's tier onto its row", async () => {
    const stale = {
      ...goldenSnapshot,
      quota: [
        { ...goldenSnapshot.quota[0], fresh: false },
        {
          host: "node-a",
          pool: "commandcode",
          account: "commandcode:747042b3498054b9",
          windows: [{ window: "7d", used_pct: 5, resets_at: "2026-11-20T00:00:00Z" }],
          measured_at: "2026-11-15T11:59:00Z",
          age_s: 60,
          fresh_ttl_s: 300,
          fresh: true,
        },
      ],
    };
    serve(withSnapshot(stale));
    render(<FleetStatusPage />);

    const quota = await screen.findByRole("region", { name: "호스트별 쿼터" });
    expect(within(quota).getByText("오래됨")).toBeTruthy();
    expect(document.querySelector(".fs-quota[data-stale]")).toBeTruthy();
    // The admitted plan row's tier lands on the matching (host, account) row.
    expect(within(quota).getByText("ranked")).toBeTruthy();
  });

  it("renders the no-snapshot state before the hub's first write", async () => {
    serve(withSnapshot(null));
    render(<FleetStatusPage />);
    expect(await screen.findByText(/아직 플릿 스냅샷이 없습니다/)).toBeTruthy();
  });

  it("renders an alert when the API fails", async () => {
    serve({ error: "fleet_status_invalid" }, 502);
    render(<FleetStatusPage />);
    expect(await screen.findByRole("alert")).toBeTruthy();
    expect(screen.getByText(/불러오지 못했습니다/)).toBeTruthy();
  });

  it("stacks plan rows and collapses grids on a phone", () => {
    const text = css();
    const narrow = text.slice(text.indexOf("@media (max-width: 767.98px)"));
    expect(narrow.length).toBeGreaterThan(0);
    // Plan rows become two stacked areas instead of three squeezed columns.
    expect(narrow).toContain("grid-template-areas");
    expect(narrow).toContain('"grade outcome"');
    expect(narrow).toContain('"detail detail"');
    // Host cards already collapse through auto-fit; the narrow block keeps
    // the root padding and chip rows phone-sized.
    expect(narrow).toContain(".fs-root");
    expect(narrow).toContain(".fs-quota-chips");
  });
});
