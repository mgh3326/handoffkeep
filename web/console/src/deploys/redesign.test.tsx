// hk 3287 — the Deploys page redesign: one named test per behaviour in the
// brief (collapsed warning, service card, shortened refs with copy, attempt
// rows, the muted no-time chip, no inline styles).

import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { DeploysPage } from "./DeploysPage";
import type { DeployPendingResponse, DeployRecordView, DeployServiceView } from "./deploy";

const SHA = "92ee9e2d0ddc47593677adcc9e83040dd9873463";
const DIGEST = "sha256:e0580273a4c31cd56a1028389212a1e101c40f130231f6dd2c49b3b5bc0214f6";

const rec = (over: Partial<DeployRecordView> = {}): DeployRecordView => ({
  record_key: "deploy/example-console/20261101T090000Z",
  result: "success",
  deployed_ref: SHA,
  deployed_at: "2026-11-01T09:00:00Z",
  failed_step: null,
  recorded_at: "2026-11-01T09:00:00Z",
  source: "installer",
  ...over,
});

const service = (over: Partial<DeployServiceView> = {}): DeployServiceView => ({
  service: "example-console",
  repo: "example/console",
  target: "host-a",
  doc_url: "/ui/doc/deploy/example-console/",
  record_count: 1,
  current: rec(),
  recent: [rec()],
  merged_boundary: "deployed_at",
  merged_since: [],
  ...over,
});

const payload = (services: DeployServiceView[]): DeployPendingResponse => ({
  generated_at: "2026-11-01T12:00:00Z",
  pr_source: "refs.pr",
  services,
});

const show = (services: DeployServiceView[]) =>
  render(<DeploysPage fetchStatus={async () => payload(services)} pollMs={600_000} />);

afterEach(() => {
  vi.restoreAllMocks();
  Reflect.deleteProperty(navigator, "clipboard");
});

const invalidRows = (n: number) =>
  Array.from({ length: n }, (_, i) => ({
    key: `deploy/example-console/bad-${String(i).padStart(2, "0")}`,
    prefix: "deploy/example-console/",
    reasons: i % 2 === 0 ? ["key"] : ["schema", "deployed_at"],
  }));

describe("malformed records collapse", () => {
  it("is one collapsed line by default: 'N개 기록 형식 불일치 · 펼치기'", async () => {
    show([service({ invalid_count: 40, invalid: invalidRows(40) })]);
    const warning = await screen.findByTestId("deploy-invalid-warning");
    expect(warning.hasAttribute("open")).toBe(false);
    expect(within(warning).getByText("40개 기록 형식 불일치 · 펼치기")).toBeTruthy();
    expect(within(warning).getByText("40개 기록 형식 불일치 · 펼치기").closest("summary")).toBeTruthy();
  });

  it("opens to a key and reason list, with counts per reason first", async () => {
    show([service({ invalid_count: 3, invalid: invalidRows(3) })]);
    const warning = await screen.findByTestId("deploy-invalid-warning");
    fireEvent.click(within(warning).getByText(/펼치기/));
    expect(warning.hasAttribute("open")).toBe(true);
    await waitFor(() => expect(within(warning).getByText("3개 기록 형식 불일치 · 접기")).toBeTruthy());
    const list = within(warning).getByTestId("deploy-invalid-list");
    expect(within(list).getAllByRole("listitem")).toHaveLength(3);
    expect(list.textContent).toContain("deploy/example-console/bad-00");
    expect(list.textContent).toContain("접두사: deploy/example-console/");
    expect(list.textContent).toContain("키 시각 형식이 계약(YYYYMMDDTHHMMSSZ)이 아님");
    expect(warning.textContent).toContain("키 시각 형식이 계약(YYYYMMDDTHHMMSSZ)이 아님 2건");
    expect(warning.textContent).toContain("deploy-record/v0 본문이 아님(schema·service·result) 1건");
  });

  it("lists 20 keys, then the rest on '더 보기'", async () => {
    show([service({ invalid_count: 40, invalid: invalidRows(40) })]);
    const warning = await screen.findByTestId("deploy-invalid-warning");
    expect(within(warning).getAllByRole("listitem")).toHaveLength(20);
    fireEvent.click(within(warning).getByRole("button", { name: "20개 더 보기" }));
    expect(within(warning).getAllByRole("listitem")).toHaveLength(40);
    expect(within(warning).queryByRole("button", { name: /더 보기/ })).toBeNull();
  });

  it("keeps the summary a native keyboard-operable control", async () => {
    show([service({ invalid_count: 1, invalid: invalidRows(1) })]);
    const summary = (await screen.findByTestId("deploy-invalid-warning")).querySelector("summary");
    expect(summary?.tagName).toBe("SUMMARY");
    expect(summary?.hasAttribute("tabindex")).toBe(false);
  });
});

describe("service card", () => {
  it("shows name, host, current version chip, last attempt status chip and time", async () => {
    show([
      service({
        recent: [rec({ result: "failed", deployed_ref: null, deployed_at: "2026-11-01T11:00:00Z", failed_step: 4 })],
        latest: rec({ result: "failed", deployed_ref: null, deployed_at: "2026-11-01T11:00:00Z", failed_step: 4 }),
      }),
    ]);
    const card = await screen.findByRole("region", { name: "example-console 배포" });
    expect(within(card).getByRole("heading", { name: "example-console" })).toBeTruthy();
    expect(within(card).getByText("host-a")).toBeTruthy();
    const status = card.querySelector(".dp-status") as HTMLElement;
    expect(within(status).getByText("현재 판")).toBeTruthy();
    expect(within(status).getByText("92ee9e2d0ddc")).toBeTruthy();
    expect(within(status).getByText("마지막 시도")).toBeTruthy();
    const chip = status.querySelector('[data-chip="status"]') as HTMLElement;
    expect(chip.textContent).toBe("실패");
    expect(chip.getAttribute("data-group")).toBe("failed");
    expect(chip.querySelector('[data-icon="warning"]')).toBeTruthy();
    expect(within(status).getByText("1시간 전").getAttribute("title")).toBe("01/11, 20:00 KST");
  });

  it("a success is the done group and still prints its word", async () => {
    show([service()]);
    const status = (await screen.findByRole("region", { name: "example-console 배포" })).querySelector(".dp-status") as HTMLElement;
    const chip = status.querySelector('[data-chip="status"]') as HTMLElement;
    expect(chip.textContent).toBe("성공");
    expect(chip.getAttribute("data-group")).toBe("done");
  });
});

describe("shortened refs with a copy button", () => {
  it("cuts a SHA and a digest to 12 in Mono and keeps the full value in the title", async () => {
    show([
      service({ current: rec({ deployed_ref: SHA }), recent: [] }),
      service({ service: "other", current: rec({ deployed_ref: DIGEST }), recent: [] }),
    ]);
    const sha = await screen.findByText("92ee9e2d0ddc");
    expect(sha.className).toContain("hk-mono");
    expect(sha.parentElement?.getAttribute("title")).toBe(SHA);
    const digest = screen.getByText("sha256:e0580273a4c3…");
    expect(digest.className).toContain("hk-mono");
    expect(digest.parentElement?.getAttribute("title")).toBe(DIGEST);
  });

  it("a version string is shown whole, with no copy button", async () => {
    show([service({ current: rec({ deployed_ref: "pw-e401923" }), recent: [] })]);
    await screen.findByText("pw-e401923");
    expect(screen.queryByRole("button", { name: /복사/ })).toBeNull();
  });

  it("the copy button has an accessible name, copies the full value and announces it", async () => {
    const writeText = vi.fn(async () => {});
    Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
    const spoken: string[] = [];
    const listen = (e: Event) => spoken.push((e as CustomEvent<string>).detail);
    window.addEventListener("hk-console-announce", listen);
    show([service({ recent: [] })]);
    const button = await screen.findByRole("button", { name: "example-console 현재 판 전체 값 복사" });
    expect(button.getAttribute("type")).toBe("button");
    await act(async () => { fireEvent.click(button); });
    expect(writeText).toHaveBeenCalledWith(SHA);
    expect(spoken).toEqual(["복사함"]);
    expect(button.querySelector('[data-icon="check"]')).toBeTruthy();
    window.removeEventListener("hk-console-announce", listen);
  });

  it("a refused copy says so instead of claiming success", async () => {
    Object.defineProperty(navigator, "clipboard", { value: { writeText: vi.fn(async () => { throw new Error("denied"); }) }, configurable: true });
    const spoken: string[] = [];
    const listen = (e: Event) => spoken.push((e as CustomEvent<string>).detail);
    window.addEventListener("hk-console-announce", listen);
    show([service({ recent: [] })]);
    const button = await screen.findByRole("button", { name: /전체 값 복사/ });
    await act(async () => { fireEvent.click(button); });
    expect(spoken).toEqual(["복사하지 못함"]);
    expect(button.querySelector('[data-icon="copy"]')).toBeTruthy();
    window.removeEventListener("hk-console-announce", listen);
  });
});

describe("recent attempts as rows", () => {
  const attempts = Array.from({ length: 5 }, (_, i) =>
    rec({ record_key: `deploy/example-console/2026110${i + 1}T090000Z`, result: i === 1 ? "rolled_back" : i === 2 ? "failed" : "success", deployed_ref: i === 0 ? SHA : null, deployed_at: `2026-11-0${i + 1}T09:00:00Z` }),
  );

  it("each row has a status icon and word, a version, a record link and a relative time", async () => {
    show([service({ recent: attempts })]);
    const list = await screen.findByLabelText("example-console 최근 배포");
    const row = within(list).getAllByRole("listitem")[0];
    expect(row.querySelector('[data-chip="status"] svg')).toBeTruthy();
    expect(row.textContent).toContain("성공");
    expect(within(row).getByText("92ee9e2d0ddc")).toBeTruthy();
    const link = within(row).getByRole("link", { name: "deploy/example-console/20261101T090000Z" });
    expect(link.getAttribute("href")).toBe("/ui/doc/deploy/example-console/20261101T090000Z");
    expect(row.querySelector("time")?.textContent).toBe("3시간 전");
  });

  it("shows three attempts, then the rest on '더 보기', and folds back", async () => {
    show([service({ recent: attempts })]);
    const list = await screen.findByLabelText("example-console 최근 배포");
    expect(within(list).getAllByRole("listitem")).toHaveLength(3);
    const more = screen.getByRole("button", { name: "나머지 2건 더 보기" });
    expect(more.getAttribute("aria-expanded")).toBe("false");
    fireEvent.click(more);
    expect(within(list).getAllByRole("listitem")).toHaveLength(5);
    const fold = screen.getByRole("button", { name: "접기" });
    expect(fold.getAttribute("aria-expanded")).toBe("true");
    fireEvent.click(fold);
    expect(within(list).getAllByRole("listitem")).toHaveLength(3);
  });

  it("no button when the attempts fit", async () => {
    show([service({ recent: attempts.slice(0, 3) })]);
    await screen.findByLabelText("example-console 최근 배포");
    expect(screen.queryByRole("button", { name: /더 보기/ })).toBeNull();
  });
});

describe("a record without a time", () => {
  it("says '시각 미기록' as a muted chip, in the card and the row", async () => {
    const blank = rec({ deployed_at: null });
    show([service({ current: blank, recent: [blank] })]);
    const card = await screen.findByRole("region", { name: "example-console 배포" });
    const chips = within(card).getAllByText("시각 미기록");
    expect(chips).toHaveLength(2);
    for (const chip of chips) {
      expect(chip.className).toContain("hk-chip");
      expect(chip.className).toContain("dp-chip-muted");
    }
  });
});

describe("styles", () => {
  it("renders no inline style attribute anywhere on the page", async () => {
    const capped = service({ docs_capped: true, invalid_count: 2, invalid: invalidRows(2), merged_since: [{ task_id: 7, title: "A change", pr: "https://github.com/example/console/pull/9", merged_at: "2026-11-01T10:00:00Z" }] });
    show([capped, service({ service: "second", current: null, record_count: 0, recent: [], merged_boundary: "no_current" })]);
    await screen.findByText("A change");
    expect(document.querySelectorAll("[style]")).toHaveLength(0);
  });
});

describe("a service with no attempts", () => {
  it("has no empty 최근 시도 block", async () => {
    show([service({ current: null, record_count: 0, recent: [], merged_boundary: "no_current" })]);
    const card = await screen.findByRole("region", { name: "example-console 배포" });
    expect(within(card).getByText("기록 없음")).toBeTruthy();
    expect(within(card).queryByText("최근 시도")).toBeNull();
    expect(within(card).queryByRole("list", { name: /최근 배포/ })).toBeNull();
  });
});

describe("operator-facing copy", () => {
  const merged = { task_id: 7, title: "A change", pr: "https://github.com/example/console/pull/9", merged_at: "2026-11-01T10:00:00Z" };

  it("shows a merged task's time as relative, with the absolute time in the title", async () => {
    show([service({ merged_since: [merged] })]);
    const item = (await screen.findByText("A change")).closest("li") as HTMLElement;
    const time = item.querySelector("time") as HTMLElement;
    expect(time.textContent).toBe("2시간 전");
    expect(time.getAttribute("title")).toBe("01/11, 19:00 KST");
    expect(item.textContent).not.toMatch(/\d\d\/\d\d, \d\d:\d\d/);
  });

  it("the footnote names no internal field or issue number", async () => {
    show([service()]);
    const note = (await screen.findByText(/배포가 필요 없는 머지/)) as HTMLElement;
    expect(note.textContent).not.toMatch(/refs\.pr|종결|#494/);
  });
});

 it("renders all five registered services", async () => {
  const names = ["handoffkeep", "auto_trader", "panewire-hub", "mergewake", "pacelever"];
  show(names.map((name) => service({ service: name, repo: `mgh3326/${name}` })));
  for (const name of names) expect(await screen.findByRole("region", { name: `${name} 배포` })).toBeTruthy();
 });
