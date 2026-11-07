import { useEffect, useRef, useState } from "react";
import {
  closeTaskCompleted, closeTaskMerged, dropTask, fetchLanes, holdTask, relaneTask, reopenTask,
  TaskActionError, taskActionMessage,
} from "../boardApi";
import { stateLabel } from "../states";
import type { Action, ActionTarget } from "./index";
import { ActionDialog } from "./dialogs";

function announce(message: string) {
  window.dispatchEvent(new CustomEvent("hk-console-announce", { detail: message }));
}

function undoLater(message: string, undo: () => Promise<void>) {
  window.dispatchEvent(new CustomEvent("hk-lifecycle-undo", { detail: { message, undo } }));
}

function failed(error: unknown) {
  const code = error instanceof TaskActionError ? error.code : "network";
  window.dispatchEvent(new CustomEvent("hk-lifecycle-error", { detail: taskActionMessage(code) }));
}

/** Tells the lists that a task changed so they reload before the next poll. */
function changed(id: number) {
  window.dispatchEvent(new CustomEvent("hk-task-changed", { detail: { id } }));
}

/** Renders the dialog or the immediate action for one registry entry. */
export function LifecycleAction({ action, target, close, onApplied }: { action: Action; target: ActionTarget; close: () => void; onApplied: () => void }) {
  switch (action.id) {
    case "hold":
      return <HoldAction target={target} close={close} onApplied={onApplied} />;
    case "relane":
      return <RelaneDialog target={target} close={close} onApplied={onApplied} />;
    default:
      return <ConfirmAction action={action} target={target} close={close} onApplied={onApplied} />;
  }
}

function ConfirmAction({ action, target, close, onApplied }: { action: Action; target: ActionTarget; close: () => void; onApplied: () => void }) {
  const [pending, setPending] = useState(false);
  const [evidence, setEvidence] = useState(target.refs?.pr ?? "");
  const [reason, setReason] = useState("");
  const [closure, setClosure] = useState("cancelled");
  const [duplicateOf, setDuplicateOf] = useState("");
  const run = async (work: () => Promise<string>) => {
    setPending(true);
    try {
      const message = await work();
      close();
      announce(message);
      onApplied();
      changed(target.id);
    } catch (error) {
      failed(error);
      close();
    } finally {
      setPending(false);
    }
  };
  const trimmedEvidence = evidence.trim();
  const trimmedReason = reason.trim();
  const superseded = closure === "superseded";
  const duplicateID = Number(duplicateOf.trim());
  if (action.id === "close-merged") {
    const path = target.state === "reviewing"
      ? `${stateLabel(target.state)} → ${stateLabel("merged")}`
      : `${stateLabel(target.state)} → ${stateLabel("reviewing")} → ${stateLabel("merged")}`;
    return <ActionDialog
      title="머지됨으로 닫기"
      effect="머지됨으로 닫기"
      close={close}
      pending={pending}
      confirm={() => void run(async () => {
        await closeTaskMerged(target.id, evidence, reason);
        return "머지됨으로 닫았습니다.";
      })}
    >
      <p>기록 경로: {path}</p>
      {target.state !== "reviewing" && <p>이 작업은 검토 중이 아니므로, 저장하면 검토 단계를 거쳐 한 번에 기록됩니다.</p>}
      <label>종료 근거 <input value={evidence} onChange={e => setEvidence(e.target.value)} placeholder="https://github.com/…/pull/123" /></label>
      <label>메모 (선택) <textarea value={reason} onChange={e => setReason(e.target.value)} /></label>
    </ActionDialog>;
  }
  if (action.id === "close-completed") {
    return <ActionDialog
      title="완료로 닫기"
      effect="완료로 닫기"
      close={close}
      pending={pending}
      confirmDisabled={trimmedEvidence === ""}
      confirm={() => void run(async () => {
        await closeTaskCompleted(target.id, evidence, reason);
        return "완료로 닫았습니다.";
      })}
    >
      <p>decide·ops·verify 작업만 완료로 닫을 수 있습니다.</p>
      <label>결과 링크 <input value={evidence} onChange={e => setEvidence(e.target.value)} placeholder="https://… 또는 hk:doc path/to/doc" /></label>
      <label>메모 (선택) <textarea value={reason} onChange={e => setReason(e.target.value)} /></label>
    </ActionDialog>;
  }
  if (action.id === "drop") {
    return <ActionDialog
      title="드롭 / 중복"
      effect={superseded ? "중복으로 닫기" : "드롭하기"}
      close={close}
      pending={pending}
      confirmDisabled={trimmedReason === "" || (superseded && !(Number.isSafeInteger(duplicateID) && duplicateID > 0))}
      confirm={() => void run(async () => {
        await dropTask(target.id, superseded ? "superseded" : closure === "obsolete" ? "obsolete" : "cancelled", reason, superseded ? duplicateID : 0);
        return superseded ? "중복으로 닫았습니다." : "드롭했습니다.";
      })}
    >
      <fieldset>
        <legend>종료 유형</legend>
        <label><input type="radio" name={`closure-${target.id}`} checked={closure === "cancelled"} onChange={() => setClosure("cancelled")} /> 취소</label>
        <label><input type="radio" name={`closure-${target.id}`} checked={closure === "obsolete"} onChange={() => setClosure("obsolete")} /> 더 이상 필요 없음</label>
        <label><input type="radio" name={`closure-${target.id}`} checked={superseded} onChange={() => setClosure("superseded")} /> 대체됨 (중복)</label>
      </fieldset>
      {superseded && <label>대체 작업 번호 <input inputMode="numeric" value={duplicateOf} onChange={e => setDuplicateOf(e.target.value)} placeholder="예: 3312" /></label>}
      <label>이유 <textarea autoFocus={false} value={reason} onChange={e => setReason(e.target.value)} /></label>
    </ActionDialog>;
  }
  return <ActionDialog
    title="다시 열기"
    effect="다시 열기"
    close={close}
    pending={pending}
    confirmDisabled={trimmedReason === ""}
    confirm={() => void run(async () => {
      await reopenTask(target.id, reason);
      return "작업을 다시 열었습니다.";
    })}
  >
    <p>{stateLabel(target.state)} → {stateLabel("unstarted")}</p>
    <label>이유 <textarea value={reason} onChange={e => setReason(e.target.value)} /></label>
  </ActionDialog>;
}

/** Hold and hold release take effect at once; the toast's undo restores the
 * exact state the task had before (a hold) or re-parks it (a release). */
function HoldAction({ target, close, onApplied }: { target: ActionTarget; close: () => void; onApplied: () => void }) {
  const started = useRef(false);
  useEffect(() => {
    if (started.current) return;
    started.current = true;
    const held = target.state !== "icebox";
    void holdTask(target.id, held)
      .then(result => {
        close();
        announce(held ? "보류했습니다." : "보류를 해제했습니다.");
        undoLater(held ? "보류했습니다" : "보류를 해제했습니다", async () => {
          try {
            if (held) await holdTask(target.id, false, result.previous);
            else await holdTask(target.id, true);
            onApplied();
            changed(target.id);
          } catch (error) {
            failed(error);
          }
        });
        onApplied();
        changed(target.id);
      })
      .catch(error => { failed(error); close(); });
  }, [target.id, target.state, close, onApplied]);
  return null;
}

function RelaneDialog({ target, close, onApplied }: { target: ActionTarget; close: () => void; onApplied: () => void }) {
  const [lanes, setLanes] = useState<string[] | null>(null);
  const [loadFailed, setLoadFailed] = useState(false);
  const [lane, setLane] = useState("");
  const [reason, setReason] = useState("");
  const [pending, setPending] = useState(false);
  useEffect(() => {
    let active = true;
    void fetchLanes()
      .then(list => {
        if (!active) return;
        const choices = list.filter(name => name !== target.lane);
        setLanes(choices);
        setLane(choices[0] ?? "");
      })
      .catch(() => { if (active) setLoadFailed(true); });
    return () => { active = false; };
  }, [target.lane]);
  const confirm = () => {
    setPending(true);
    void relaneTask(target.id, lane, reason)
      .then(result => {
        close();
        announce(result.changed ? "레인을 옮겼습니다." : "이미 그 레인에 있습니다.");
        if (!result.changed) return;
        if (result.relay === "failed" || result.relay === "unconfigured") failed(new TaskActionError(0, "relay_failed"));
        undoLater("레인을 옮겼습니다", async () => {
          try {
            await relaneTask(target.id, result.previous, "레인 이동 실행 취소");
            onApplied();
            changed(target.id);
          } catch (error) {
            failed(error);
          }
        });
        onApplied();
        changed(target.id);
      })
      .catch(error => { failed(error); close(); })
      .finally(() => setPending(false));
  };
  return <ActionDialog
    title="레인 넘기기"
    effect="레인 옮기기"
    close={close}
    pending={pending}
    confirmDisabled={lane === ""}
    confirm={confirm}
  >
    <p>현재 레인: {target.lane ?? "기록 없음"}</p>
    {loadFailed && <p role="alert">레인 목록을 불러오지 못했습니다.</p>}
    {!loadFailed && lanes === null && <p role="status">레인 목록 불러오는 중…</p>}
    {lanes !== null && lanes.length === 0 && <p>옮길 다른 레인이 없습니다.</p>}
    {lanes !== null && lanes.length > 0 && <label>옮길 레인 <select value={lane} onChange={e => setLane(e.target.value)}>{lanes.map(name => <option key={name} value={name}>{name}</option>)}</select></label>}
    <label>이유 (선택) <textarea value={reason} onChange={e => setReason(e.target.value)} /></label>
  </ActionDialog>;
}
