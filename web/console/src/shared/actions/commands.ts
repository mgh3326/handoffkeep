import type { Action } from "./index";

export type CommandIntent = "steer" | "queue";

/** One operator command as the console shows it. The ladder never shows a
 * state ahead of the executor's receipt. */
export type OperatorCommandView = {
  id: number;
  kind: string;
  state: string;
  intent?: string;
  text?: string;
  actor: string;
  last_error_code?: string;
  created_at: string;
  delivered_at?: string | null;
  acked_at?: string | null;
  finished_at?: string | null;
  expires_at: string;
};

/** The row chip carries only the in-flight command identity. */
export type OperatorCommandChip = {
  id: number;
  kind: string;
  state: string;
  created_at: string;
};

/**
 * Kinds the console can offer. worker.message is the tracer and has an
 * executor; the other three are reserved: the action is disabled with a
 * Korean reason until C4 (panewire stop/respawn) and C5 (deploy approval)
 * land their executors.
 */
export const commandActions: Action[] = [
  {
    id: "worker.message", label: "워커에게 지시", keywords: ["지시", "message", "instruct"], kind: "command", confirm: "dialog",
    applicable: () => true,
    disabledReason: target => target.active_job ? null : "활성 작업이 없습니다",
  },
  {
    id: "job.stop", label: "워커 중지", keywords: ["중지", "stop"], kind: "command", confirm: "dialog",
    applicable: target => !!target.active_job,
    disabledReason: () => "실행기 준비 전",
  },
  {
    id: "task.respawn", label: "작업 재시작", keywords: ["재시작", "respawn"], kind: "command", confirm: "dialog",
    applicable: () => true,
    disabledReason: () => "실행기 준비 전",
  },
  {
    id: "deploy.approve", label: "배포 승인", keywords: ["배포", "approve"], kind: "command", confirm: "typed", target: "deploy",
    applicable: () => true,
    disabledReason: () => "실행기 준비 전",
  },
];

export const commandKindLabels: Record<string, string> = {
  "worker.message": "워커에게 지시",
  "job.stop": "워커 중지",
  "task.respawn": "작업 재시작",
  "deploy.approve": "배포 승인",
};

export const commandKindShort: Record<string, string> = {
  "worker.message": "지시",
  "job.stop": "중지",
  "task.respawn": "재시작",
  "deploy.approve": "배포",
};

export const commandStateLabels: Record<string, string> = {
  recorded: "기록됨",
  delivered: "전달됨",
  acked: "접수됨",
  done: "완료",
  failed: "실패",
  expired: "만료",
  superseded: "대체됨",
};

/** Why an executor failed, in Korean. Unknown codes stay as the identifier
 * they are — operator-facing policy keeps error codes inline. */
export function commandErrorLabel(code?: string): string {
  switch (code) {
    case undefined:
    case "":
      return "오류 코드 없음";
    case "worker_rejected":
      return "워커가 거부함";
    case "turn_unavailable":
      return "턴을 잡지 못함";
    case "executor_unavailable":
      return "실행기를 찾지 못함";
    case "queue_unsupported":
      return "대기열 미지원";
    case "timeout":
      return "시간 초과";
    default:
      return `오류 코드: ${code}`;
  }
}

export function commandIntentLabel(intent?: string): string {
  return intent === "queue" ? "현재 작업 끝나고" : "지금 끼어들기";
}
