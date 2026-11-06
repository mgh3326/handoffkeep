import { act, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { matchRoute, navKey } from "../app/routes";
import { Shell } from "../app/Shell";
import { DeploysPage } from "./DeploysPage";
import type { DeployPendingResponse, DeployRecordView } from "./deploy";

const record = (i: number): DeployRecordView => ({ record_key: `deploy/a/${i}`, result: ["success", "failed", "rolled_back"][i % 3], deployed_ref: "abcdef0123456789012345678901234567890123", deployed_at: "2026-11-01T12:00:00Z", failed_step: i, recorded_at: "2026-11-01T12:00:00Z", source: "fixture", serving_maybe_changed: i === 1 });
const fixture = (): DeployPendingResponse => ({ generated_at: "2026-11-01T12:00:00Z", pr_source: "refs.pr", services: ["z", "a", "m"].map((service, index) => ({ service, repo: `example/${service}`, doc_url: `/ui/doc/deploy/${service}/`, record_count: 10, current: record(0), recent: Array.from({length: 10}, (_, i) => record(i)), merged_boundary: "deployed_at", merged_since: index === 0 ? [] : Array.from({length: index}, (_, i) => ({task_id: index * 10 + i, title: `Task ${index}-${i}`, pr: "https://github.com/example/repo/pull/1", merged_at: "2026-11-01T13:00:00Z"})) })) });
afterEach(() => { vi.useRealTimers(); vi.restoreAllMocks(); });
const settle = async () => { await act(async () => { await Promise.resolve(); await Promise.resolve(); }); };

it("D1 routes Deploys and marks its navigation link", () => {
 const route = matchRoute("/ui/deploys"); expect(route).toEqual({view: "deploys"}); expect(navKey(route)).toBe("deploys");
 render(<Shell current={navKey(route)}><div /></Shell>); expect(screen.getByRole("link", {name: "배포"}).getAttribute("aria-current")).toBe("page");
});
it("D2 renders ordered service cards and waiting task links", async () => {
 render(<DeploysPage fetchStatus={async () => fixture()} />); await screen.findByText("Task 1-0");
 expect(screen.getAllByRole("region").filter(el => el.getAttribute("aria-label")?.endsWith(" 배포") && !el.getAttribute("aria-label")?.includes("최근")).map(el => el.getAttribute("aria-label"))).toEqual(["a 배포", "m 배포", "z 배포"]);
 expect(screen.getAllByText(/마지막 배포 이후 머지됨/).length).toBe(3); expect(screen.getByText("#10").closest("a")?.getAttribute("href")).toBe("/ui/tasks/10"); expect(document.body.textContent).not.toContain("미배포 확정");
});
it("D3 renders ten recent attempts including failures and rollback", async () => {
 render(<DeploysPage fetchStatus={async () => fixture()} />); const list = await screen.findByLabelText("a 최근 배포");
 expect(list.querySelectorAll("[data-result]")).toHaveLength(3);
 fireEvent.click(screen.getAllByRole("button", {name: "나머지 7건 더 보기"})[0]);
 expect(list.querySelectorAll("[data-result]")).toHaveLength(10); for (const label of ["성공", "실패", "롤백"]) expect(within(list).getAllByText(label).length).toBeGreaterThan(0); expect(within(list).getByRole("note").textContent).toContain("서빙 판이 바뀌었을 수 있음");
});
it("D4 polls every 15 seconds and parks while hidden", async () => {
 vi.useFakeTimers(); let hidden = false; vi.spyOn(document, "hidden", "get").mockImplementation(() => hidden); const fetchStatus = vi.fn(async () => fixture()); render(<DeploysPage fetchStatus={fetchStatus} />); await settle();
 await act(async () => { await vi.advanceTimersByTimeAsync(15000); }); expect(fetchStatus).toHaveBeenCalledTimes(2);
 hidden = true; await act(async () => { await vi.advanceTimersByTimeAsync(60000); }); expect(fetchStatus).toHaveBeenCalledTimes(2);
 hidden = false; await act(async () => { document.dispatchEvent(new Event("visibilitychange")); }); expect(fetchStatus).toHaveBeenCalledTimes(3);
});
it("D5 keeps data on failure and clears stale on recovery", async () => {
 vi.useFakeTimers(); const fetchStatus = vi.fn().mockResolvedValueOnce(fixture()).mockRejectedValueOnce(new Error("offline")).mockResolvedValue(fixture()); render(<DeploysPage fetchStatus={fetchStatus} />); await settle();
 await act(async () => { await vi.advanceTimersByTimeAsync(15000); }); expect(screen.getByText(/갱신 실패 · 마지막 확인/)).toBeTruthy(); expect(screen.getByText("Task 1-0")).toBeTruthy();
 await act(async () => { await vi.advanceTimersByTimeAsync(15000); }); expect(screen.queryByText(/갱신 실패/)).toBeNull();
});
it("D6 sets the waiting title and rejects malformed history", async () => {
 document.title = "Console"; const {unmount} = render(<DeploysPage fetchStatus={async () => fixture()} />); await screen.findByText("Task 1-0"); expect(document.title).toBe("Deploys · 3 waiting"); unmount(); expect(document.title).toBe("Console");
 const malformed = fixture(); Object.assign(malformed.services[0], {recent: "bad"}); render(<DeploysPage fetchStatus={async () => malformed} />); expect(await screen.findByText(/불러오지 못했습니다/)).toBeTruthy(); expect(screen.queryByLabelText("a 배포")).toBeNull();
});
it("D7 preserves the advisory and warning content", async () => {
 const data = fixture(); data.services[0].docs_capped = true; data.services[0].invalid_count = 1; data.services[0].invalid = [{key: "bad/key", reasons: ["schema"]}]; render(<DeploysPage fetchStatus={async () => data} />); expect(await screen.findByText("bad/key")).toBeTruthy(); expect(screen.getByText(/기록 조회 상한/)).toBeTruthy();
 expect(screen.getByTestId("deploy-invalid-warning").hasAttribute("open")).toBe(false);
});
