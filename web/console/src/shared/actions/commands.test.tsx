import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { commandActions, type OperatorCommandView } from "./commands";
import { CommandLadder } from "./CommandLadder";
import { TaskActions } from "./TaskActions";

afterEach(() => { vi.restoreAllMocks(); document.querySelector('meta[name="hk-csrf"]')?.remove(); });

const target = { id: 101, state: "developing", priority: 2 };
const active = { ...target, active_job: { role: "worker" } };
const command: OperatorCommandView = {
  id: 7, kind: "worker.message", state: "recorded", actor: "operator:test", text: "테스트부터 확인",
  created_at: "2026-11-07T01:00:00Z", expires_at: "2026-11-07T01:30:00Z",
};

function registry(id: string) {
  const action = commandActions.find(a => a.id === id);
  if (!action) throw new Error(`missing action ${id}`);
  return action;
}

describe("operator command registry", () => {
  it("offers worker.message only with an active job", () => {
    expect(registry("worker.message").disabledReason(target)).toBe("활성 작업이 없습니다");
    expect(registry("worker.message").disabledReason(active)).toBeNull();
  });
  it("keeps every reserved kind disabled with the executor reason", () => {
    for (const id of ["job.stop", "task.respawn", "deploy.approve"]) {
      expect(registry(id).disabledReason(active)).toBe("실행기 준비 전");
    }
    expect(registry("deploy.approve").target).toBe("deploy");
  });
  it("renders reserved kinds disabled with the reason as their title", () => {
    render(<TaskActions target={active} onPriority={() => {}} />);
    fireEvent.click(screen.getByRole("button", { name: "작업 #101 메뉴" }));
    const stop = screen.getByRole("menuitem", { name: /워커 중지/ });
    expect((stop as HTMLButtonElement).disabled).toBe(true);
    expect(stop.getAttribute("title")).toBe("실행기 준비 전");
    expect(screen.getByRole("menuitem", { name: /워커에게 지시/ })).toBeTruthy();
  });
});

describe("command ladder", () => {
  const cases: Array<[string, Partial<OperatorCommandView>, string]> = [
    ["recorded", {}, "진행 중"],
    ["delivered", { state: "delivered", delivered_at: "2026-11-07T01:01:00Z" }, "진행 중"],
    ["acked", { state: "acked", delivered_at: "2026-11-07T01:01:00Z", acked_at: "2026-11-07T01:02:00Z" }, "진행 중"],
    ["done", { state: "done", delivered_at: "2026-11-07T01:01:00Z", acked_at: "2026-11-07T01:02:00Z", finished_at: "2026-11-07T01:05:00Z" }, "완료"],
    ["failed", { state: "failed", delivered_at: "2026-11-07T01:01:00Z", acked_at: "2026-11-07T01:02:00Z", finished_at: "2026-11-07T01:05:00Z", last_error_code: "worker_rejected" }, "실패 · 워커가 거부함"],
    ["expired", { state: "expired", finished_at: "2026-11-07T01:30:00Z" }, "만료"],
    ["superseded", { state: "superseded", finished_at: "2026-11-07T01:20:00Z" }, "대체됨"],
  ];
  for (const [name, patch, label] of cases) {
    it(`renders the ${name} ladder`, () => {
      render(<CommandLadder command={{ ...command, ...patch }} />);
      expect(screen.getByLabelText("명령 #7 진행 단계")).toBeTruthy();
      expect(screen.getByText(label)).toBeTruthy();
      expect(screen.getAllByRole("listitem")).toHaveLength(4);
    });
  }
  it("shows the receipt note and unknown error codes as identifiers", () => {
    render(<CommandLadder command={{ ...command, state: "failed", last_error_code: "odd_code" }} />);
    expect(screen.getByText("실패 · 오류 코드: odd_code")).toBeTruthy();
  });
});

describe("instruct dialog", () => {
  it("records steer and queue instructions", async () => {
    const meta = document.createElement("meta"); meta.name = "hk-csrf"; meta.content = "token"; document.head.append(meta);
    const recorded = vi.fn();
    const fetcher = vi.spyOn(window, "fetch").mockResolvedValue(new Response(JSON.stringify({ ...command, intent: "queue" }), { status: 201 }));
    render(<div data-action-task="101"><TaskActions compact target={active} onPriority={() => {}} onCommand={recorded} /></div>);
    fireEvent.click(screen.getByRole("button", { name: "작업 #101 메뉴" }));
    fireEvent.click(screen.getByRole("menuitem", { name: /워커에게 지시/ }));
    const dialog = screen.getByRole("dialog", { name: "워커에게 지시" });
    expect(dialog.textContent).toContain("지금 끼어들기");
    fireEvent.click(screen.getByRole("radio", { name: "현재 작업 끝나고" }));
    fireEvent.change(screen.getByLabelText("지시 내용"), { target: { value: "테스트부터 확인" } });
    fireEvent.click(screen.getByRole("button", { name: "명령 기록" }));
    await waitFor(() => expect(recorded).toHaveBeenCalledOnce());
    const body = JSON.parse(String(fetcher.mock.calls[0][1]?.body));
    expect(body).toMatchObject({ kind: "worker.message", text: "테스트부터 확인", intent: "queue" });
    expect(typeof body.event_key).toBe("string");
    expect(screen.queryByRole("dialog")).toBeNull();
  });
});
