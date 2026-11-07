// Applies parsed changes through the existing C1/C2/C6 console routes. There
// is no new write path here: each change is one call to the same function the
// row menu uses, with its own idempotency key. A change that cannot run is
// skipped and reported; a failed call does not hide the ones that succeeded.
// Undo exists only where the action has an exact inverse (priority, lane,
// hold/unhold); closing and commenting have none.

import {
  closeTaskCompleted,
  closeTaskMerged,
  dropTask,
  holdTask,
  newEventKey,
  postTaskComment,
  readCsrfToken,
  relaneTask,
  setTaskPriority,
  CommentWriteError,
  TaskActionError,
  taskActionMessage,
} from "../boardApi";
import type { ActionTarget } from "../actions";
import type { CommandChange } from "./grammar";
import { previewCommand, type PreviewContext, type PreviewRow } from "./preview";

export type AppliedChange = { target: ActionTarget; change: CommandChange; key: string };
export type FailedChange = { target: ActionTarget; change: CommandChange; message: string };

export type UndoResult = { restored: number; failed: FailedChange[]; summary: string };

export type ApplyResult = {
  applied: AppliedChange[];
  refused: PreviewRow[];
  failed: FailedChange[];
  summary: string;
  undo: (() => Promise<UndoResult>) | null;
};

function commandErrorMessage(error: unknown): string {
  if (error instanceof TaskActionError || error instanceof CommentWriteError) {
    const mapped = taskActionMessage(error.code);
    return mapped === "저장하지 못했습니다." && error.code !== "" ? `${mapped} (${error.code})` : mapped;
  }
  return "저장하지 못했습니다. 잠시 후 다시 시도하세요.";
}

type UndoOp = { target: ActionTarget; change: CommandChange; run: () => Promise<void> };

async function applyOne(target: ActionTarget, change: CommandChange, key: string): Promise<UndoOp | null> {
  switch (change.kind) {
    case "priority": {
      const result = await setTaskPriority(target.id, change.value, key);
      return {
        target,
        change,
        run: async () => {
          await setTaskPriority(target.id, result.previous, newEventKey());
        },
      };
    }
    case "lane": {
      const result = await relaneTask(target.id, change.lane, "명령 바에서 레인 이동", key);
      return {
        target,
        change,
        run: async () => {
          await relaneTask(target.id, result.previous, "명령 실행 취소", newEventKey(), key);
        },
      };
    }
    case "hold": {
      const result = await holdTask(target.id, change.held, "", key);
      return {
        target,
        change,
        run: async () => {
          if (change.held) await holdTask(target.id, false, result.previous, newEventKey());
          else await holdTask(target.id, true, "", newEventKey());
        },
      };
    }
    case "merged":
      await closeTaskMerged(target.id, change.evidence || target.refs?.pr || "", "", key);
      return null;
    case "drop":
      await dropTask(target.id, change.closure, change.reason, change.duplicateOf, key);
      return null;
    case "comment": {
      const csrf = readCsrfToken();
      if (!csrf) throw new CommentWriteError(0, "missing_csrf");
      await postTaskComment(target.id, change.text, csrf);
      return null;
    }
    case "state":
      switch (change.state) {
        case "icebox": {
          const result = await holdTask(target.id, true, "", key);
          return {
            target,
            change,
            run: async () => {
              await holdTask(target.id, false, result.previous, newEventKey());
            },
          };
        }
        case "merged":
          await closeTaskMerged(target.id, change.evidence || target.refs?.pr || "", "", key);
          return null;
        case "completed":
          await closeTaskCompleted(target.id, change.evidence || target.refs?.pr || "", "", key);
          return null;
        default:
          throw new TaskActionError(0, "unsupported_state");
      }
  }
}

export async function applyCommand(targets: ActionTarget[], changes: CommandChange[], context: PreviewContext): Promise<ApplyResult> {
  const preview = previewCommand(targets, changes, context);
  const refused = preview.rows.filter(row => row.refusal !== null);
  const runnable = preview.rows.filter(row => row.refusal === null);
  const applied: AppliedChange[] = [];
  const failed: FailedChange[] = [];
  const undoOps: UndoOp[] = [];

  // Sequential: the outcome (and the request order) stays deterministic, and
  // two changes on one task can depend on each other.
  for (const row of runnable) {
    const key = newEventKey();
    try {
      const undo = await applyOne(row.target, row.change, key);
      applied.push({ target: row.target, change: row.change, key });
      if (undo) undoOps.push(undo);
    } catch (error) {
      failed.push({ target: row.target, change: row.change, message: commandErrorMessage(error) });
    }
  }

  const undo = undoOps.length === 0 ? null : async (): Promise<UndoResult> => {
    const undoFailed: FailedChange[] = [];
    let restored = 0;
    for (const op of [...undoOps].reverse()) {
      try {
        await op.run();
        restored += 1;
      } catch (error) {
        undoFailed.push({ target: op.target, change: op.change, message: commandErrorMessage(error) });
      }
    }
    return {
      restored,
      failed: undoFailed,
      summary: undoFailed.length === 0 ? `${restored}건을 실행 취소했습니다.` : `${restored}건을 실행 취소 · ${undoFailed.length}건 실패`,
    };
  };

  return { applied, refused, failed, summary: summaryFor(applied.length, refused.length, failed.length), undo };
}

function summaryFor(applied: number, refused: number, failed: number): string {
  const parts: string[] = [];
  if (applied > 0) parts.push(`${applied}건 적용`);
  if (failed > 0) parts.push(`${failed}건 실패`);
  if (refused > 0) parts.push(`${refused}건 거부`);
  return parts.length === 0 ? "적용한 변경이 없습니다." : `명령 적용: ${parts.join(" · ")}`;
}
