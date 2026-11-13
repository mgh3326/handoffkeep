import { stateLabel } from "./states";

/** A task's panewire spawn request record (hk 4377), as the server shows it on
 * the task and in the work status. The console shows it; it never derives the
 * state from anything else. */
export type TaskSpawnStart = {
  task_id: number;
  /** The level the condition waits for: the blocking task merged, or deployed. */
  until: "merged" | "deployed" | string;
  /** The blocking task's current state; empty when it is unknown. */
  task_state: string;
  /** Whether the condition holds now, as the server judged it. */
  met: boolean;
};

export type TaskSpawn = {
  request_id: string;
  job: string;
  role: string;
  state: string;
  reason: string;
  attempt: number;
  host: string;
  next_check_at: string | null;
  expires_at: string | null;
  updated_at: string;
  start_condition: TaskSpawnStart | null;
};

const SPAWN_LABEL: Record<string, string> = {
  waiting: "조건 대기",
  pending: "배치 대기",
  parked: "보류",
  bound: "호스트 배정됨",
  launching: "기동 중",
  reconciling: "결과 확인 중",
  landed: "기동 완료",
  refused: "거부됨",
  given_up: "포기함",
  cancelled: "취소됨",
};

export function spawnStateLabel(state: string): string {
  return Object.prototype.hasOwnProperty.call(SPAWN_LABEL, state)
    ? SPAWN_LABEL[state]
    : `알 수 없는 요청 상태 (${state})`;
}

/** The start condition in one line: the blocking task, its state, the level it
 * waits for, and whether the condition holds. */
export function spawnStartText(start: TaskSpawnStart): string {
  const level = start.until === "deployed" ? "배포 후" : "머지 후";
  const blocker = start.task_state ? stateLabel(start.task_state) : "상태 미확인";
  return `#${start.task_id} (${blocker}) ${level} 시작 · ${start.met ? "충족" : "미충족"}`;
}
