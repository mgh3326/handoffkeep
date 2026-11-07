// What the command bar shows before anything is sent: for each target and
// each parsed change, the old value, the new value, and the reason the store
// will refuse it. Refusals come from the C1 registry's enabled/disabled
// reason, so the preview and the row menu can never disagree.

import { actions } from "../actions";
import type { ActionTarget } from "../actions";
import { stateLabel } from "../states";
import {
  CLOSURE_LABEL,
  ROUTED_STATES,
  priorityLabel,
  type CommandChange,
} from "./grammar";

export type PreviewRow = {
  target: ActionTarget;
  change: CommandChange;
  field: string;
  from: string;
  to: string;
  /** null when the change can run; a Korean sentence when it cannot. */
  refusal: string | null;
};

export type PreviewContext = { lanes: string[] };

export type Preview = { rows: PreviewRow[]; runnable: number; refused: number };

function action(id: string) {
  return actions.find(entry => entry.id === id);
}

function registryReason(id: string, target: ActionTarget): string | null {
  const entry = action(id);
  if (!entry) return "이 동작은 콘솔에서 제공하지 않습니다.";
  if (!entry.applicable(target)) return "이 작업에는 적용할 수 없습니다.";
  return entry.disabledReason(target);
}

function evidenceFor(change: Extract<CommandChange, { kind: "merged" }>, target: ActionTarget): string {
  return change.evidence || target.refs?.pr || "";
}

function truncate(text: string, limit = 60): string {
  return text.length > limit ? `${text.slice(0, limit)}…` : text;
}

function cell(target: ActionTarget, change: CommandChange, field: string, from: string, to: string, refusal: string | null): PreviewRow {
  return { target, change, field, from, to, refusal };
}

export function previewChange(target: ActionTarget, change: CommandChange, context: PreviewContext): PreviewRow {
  const oldState = stateLabel(target.state);
  switch (change.kind) {
    case "priority":
      return cell(target, change, "우선순위", priorityLabel(target.priority), priorityLabel(change.value), registryReason("priority", target));
    case "lane": {
      let refusal = registryReason("relane", target);
      if (!refusal && change.lane === target.lane) refusal = "이미 그 레인에 있습니다.";
      if (!refusal && context.lanes.length > 0 && !context.lanes.includes(change.lane)) refusal = "알 수 없는 레인입니다.";
      return cell(target, change, "레인", target.lane || "기록 없음", change.lane, refusal);
    }
    case "hold":
      if (change.held) {
        let refusal = registryReason("hold", target);
        if (!refusal && target.state === "icebox") refusal = "이미 보류 중입니다.";
        return cell(target, change, "보류", oldState, "보류", refusal);
      }
      return cell(target, change, "보류", "보류", "보류 해제", target.state === "icebox" ? registryReason("hold", target) : "보류 중인 작업이 아닙니다.");
    case "merged": {
      let refusal = registryReason("close-merged", target);
      const evidence = evidenceFor(change, target);
      if (!refusal && evidence === "") refusal = "머지 근거(PR 링크)가 필요합니다.";
      return cell(target, change, "상태", oldState, "머지됨", refusal);
    }
    case "drop": {
      let refusal = registryReason("drop", target);
      if (!refusal && change.reason.trim() === "") refusal = "드롭 이유가 필요합니다.";
      if (!refusal && change.closure === "superseded" && change.duplicateOf <= 0) refusal = "대체 작업 번호가 필요합니다.";
      return cell(target, change, "상태", oldState, `종료됨 (${CLOSURE_LABEL[change.closure]})`, refusal);
    }
    case "comment":
      return cell(target, change, "코멘트", "—", truncate(change.text), null);
    case "state": {
      if (!ROUTED_STATES.has(change.state)) {
        return cell(target, change, "상태", oldState, stateLabel(change.state), "이 상태로 바꾸는 콘솔 경로가 없습니다.");
      }
      if (change.state === target.state) {
        return cell(target, change, "상태", oldState, stateLabel(change.state), "이미 그 상태입니다.");
      }
      switch (change.state) {
        case "icebox":
          return cell(target, change, "상태", oldState, "보류", registryReason("hold", target));
        case "unstarted": {
          const refusal = registryReason("reopen", target) ?? "다시 열기는 작업 메뉴에서 이유와 함께 실행하세요.";
          return cell(target, change, "상태", oldState, "착수 전", refusal);
        }
        case "merged": {
          let refusal = registryReason("close-merged", target);
          const evidence = change.evidence || target.refs?.pr || "";
          if (!refusal && evidence === "") refusal = "머지 근거(PR 링크)가 필요합니다.";
          return cell(target, change, "상태", oldState, "머지됨", refusal);
        }
        case "completed": {
          let refusal = registryReason("close-completed", target);
          const evidence = change.evidence || target.refs?.pr || "";
          if (!refusal && evidence === "") refusal = "완료 근거(결과 링크)가 필요합니다.";
          return cell(target, change, "상태", oldState, "완료됨", refusal);
        }
        case "dropped":
        case "duplicate":
          return cell(target, change, "상태", oldState, stateLabel(change.state), "드롭 명령을 사용하세요.");
        default:
          return cell(target, change, "상태", oldState, stateLabel(change.state), "이 상태로 바꾸는 콘솔 경로가 없습니다.");
      }
    }
  }
}

export function previewCommand(targets: ActionTarget[], changes: CommandChange[], context: PreviewContext): Preview {
  const rows: PreviewRow[] = [];
  for (const target of targets) {
    for (const change of changes) rows.push(previewChange(target, change, context));
  }
  const runnable = rows.filter(row => row.refusal === null).length;
  return { rows, runnable, refused: rows.length - runnable };
}
