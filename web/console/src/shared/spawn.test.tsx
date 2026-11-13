import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { SpawnBlock } from "./SpawnBlock";
import { spawnStartText, spawnStateLabel, type TaskSpawn } from "./spawn";

const base: TaskSpawn = {
  request_id: "req-1",
  job: "hk-job-1",
  role: "worker",
  state: "parked",
  reason: "host busy",
  attempt: 2,
  host: "desktop",
  next_check_at: "2026-11-13T00:00:00Z",
  expires_at: null,
  updated_at: "2026-11-12T23:00:00Z",
  start_condition: null,
};

describe("spawn labels", () => {
  it("labels every spawn state in Korean and names an unknown one", () => {
    expect(spawnStateLabel("landed")).toBe("기동 완료");
    expect(spawnStateLabel("given_up")).toBe("포기함");
    expect(spawnStateLabel("queued")).toBe("알 수 없는 요청 상태 (queued)");
  });

  it("says which level the start condition waits for and whether it holds", () => {
    expect(spawnStartText({ task_id: 9, until: "deployed", task_state: "merged", met: false })).toBe(
      "#9 (머지됨) 배포 후 시작 · 미충족",
    );
    expect(spawnStartText({ task_id: 9, until: "merged", task_state: "", met: true })).toBe(
      "#9 (상태 미확인) 머지 후 시작 · 충족",
    );
  });
});

describe("SpawnBlock", () => {
  it("shows the state, reason, next try, host and job, and the attempt", () => {
    render(<SpawnBlock spawn={base} />);
    expect(screen.getByText("보류")).toBeTruthy();
    expect(screen.getByText("host busy")).toBeTruthy();
    expect(screen.getByText("desktop · hk-job-1")).toBeTruthy();
    expect(screen.getByText("2")).toBeTruthy();
    expect(screen.queryByText("시작 조건")).toBeNull();
  });

  it("shows the start condition when there is one", () => {
    render(<SpawnBlock spawn={{ ...base, start_condition: { task_id: 4, until: "merged", task_state: "developing", met: false } }} />);
    expect(screen.getByText("시작 조건")).toBeTruthy();
    expect(screen.getByText("#4 (개발 중) 머지 후 시작 · 미충족")).toBeTruthy();
  });
});
