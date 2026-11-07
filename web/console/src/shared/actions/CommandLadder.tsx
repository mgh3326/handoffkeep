import { useRef, useState } from "react";
import { newEventKey, sendWorkerMessage, CommandWriteError } from "../boardApi";
import { formatMinuteKST } from "../time";
import {
  commandErrorLabel,
  commandIntentLabel,
  commandKindLabels,
  commandKindShort,
  commandStateLabels,
  type CommandIntent,
  type OperatorCommandChip,
  type OperatorCommandView,
} from "./commands";
import "./commands.css";

const commandWriteMessages: Record<string, string> = {
  operator_command_no_executor: "이 작업에 실행 중인 워커가 없습니다.",
  operator_command_not_executable: "이 명령의 실행기가 아직 준비되지 않았습니다.",
  secret_like_content: "비밀 정보로 보이는 내용이 있어 기록하지 못했습니다.",
  invalid_request: "지시 내용을 확인해 주세요.",
};

function commandWriteMessage(error: unknown): string {
  if (error instanceof CommandWriteError && error.code && commandWriteMessages[error.code]) return commandWriteMessages[error.code];
  return "명령을 기록하지 못했습니다.";
}

/** The instruct dialog records a worker.message command: hk never messages the
 * worker itself. The console waits for the executor's receipt before it can
 * show anything past 기록됨. */
export function InstructDialog({ taskId, close, recorded }: { taskId: number; close: () => void; recorded: (command: OperatorCommandView) => void }) {
  const [text, setText] = useState("");
  const [intent, setIntent] = useState<CommandIntent>("steer");
  const [error, setError] = useState<string | null>(null);
  const [pending, setPending] = useState(false);
  // One key per submitted intent: an unchanged retry after a lost response
  // replays the recorded command instead of recording a superseding second one.
  const keyRef = useRef<{ signature: string; key: string } | null>(null);
  const submit = async () => {
    if (!text.trim() || pending) return;
    setPending(true); setError(null);
    try {
      const body = text.trim();
      const signature = JSON.stringify([body, intent]);
      let attempt = keyRef.current;
      if (attempt?.signature !== signature) {
        attempt = { signature, key: newEventKey() };
        keyRef.current = attempt;
      }
      const command = await sendWorkerMessage(taskId, body, intent, attempt.key);
      recorded(command);
      window.dispatchEvent(new CustomEvent("hk-console-announce", { detail: "명령을 기록했습니다. 실행기 접수 전에는 완료로 표시되지 않습니다." }));
      close();
    } catch (e) {
      setError(commandWriteMessage(e));
    } finally {
      setPending(false);
    }
  };
  return <dialog open aria-label="워커에게 지시" className="command-dialog" onKeyDown={e => { if (e.key === "Escape") { e.preventDefault(); close(); } }}>
    <form method="dialog" onSubmit={e => { e.preventDefault(); void submit(); }}>
      <h2>워커에게 지시</h2>
      <p className="command-dialog-hint">hk는 명령만 기록합니다. 워커 레인이 실행하고 접수하면 단계가 올라갑니다.</p>
      <label>지시 내용<textarea autoFocus value={text} onChange={e => setText(e.target.value)} rows={4} maxLength={1000} placeholder="예: 테스트 실패 원인부터 확인해 주세요" /></label>
      <fieldset>
        <legend>전달 방식</legend>
        <label><input type="radio" name="intent" checked={intent === "steer"} onChange={() => setIntent("steer")} /> 지금 끼어들기</label>
        <label><input type="radio" name="intent" checked={intent === "queue"} onChange={() => setIntent("queue")} /> 현재 작업 끝나고</label>
      </fieldset>
      {intent === "queue" && <p className="command-dialog-hint">대기열을 지원하지 않는 실행기는 지금 끼어들기로 처리하고 접수에 그렇게 기록합니다.</p>}
      {error && <p role="alert">{error}</p>}
      <div className="command-dialog-actions">
        <button type="button" onClick={close}>취소</button>
        <button type="submit" disabled={pending || !text.trim()}>{pending ? "기록 중…" : "명령 기록"}</button>
      </div>
    </form>
  </dialog>;
}

/** The row chip: an in-flight command on a task, never a completed one. */
export function CommandChip({ command }: { command: OperatorCommandChip }) {
  const label = `${commandKindShort[command.kind] ?? commandKindLabels[command.kind] ?? command.kind} · ${commandStateLabels[command.state] ?? command.state}`;
  return <span className="hk-chip work-command-chip" title={`${commandKindLabels[command.kind] ?? command.kind} · ${commandStateLabels[command.state] ?? command.state} · ${formatMinuteKST(command.created_at)}`}>{label}</span>;
}

type LadderStep = { key: string; label: string; at?: string | null; reached: boolean };

function ladderSteps(command: OperatorCommandView): LadderStep[] {
  const terminal = ["done", "failed", "expired", "superseded"].includes(command.state);
  const terminalLabel = commandStateLabels[command.state] ?? "진행 중";
  return [
    { key: "recorded", label: commandStateLabels.recorded, at: command.created_at, reached: true },
    { key: "delivered", label: commandStateLabels.delivered, at: command.delivered_at, reached: !!command.delivered_at },
    { key: "acked", label: commandStateLabels.acked, at: command.acked_at, reached: !!command.acked_at },
    { key: "terminal", label: terminal ? terminalLabel : "진행 중", at: terminal ? command.finished_at : null, reached: terminal },
  ];
}

/** The task detail ladder: 기록됨 → 전달됨 → 접수됨 → 완료 | 실패 | 만료,
 * with the time each step happened and the failure code in Korean. */
export function CommandLadder({ command }: { command: OperatorCommandView }) {
  const steps = ladderSteps(command);
  const current = steps.reduce((last, step, index) => (step.reached ? index : last), 0);
  return <div className="command-ladder" aria-label={`명령 #${command.id} 진행 단계`}>
    <p className="command-ladder-heading"><strong>{commandKindLabels[command.kind] ?? command.kind}</strong>{command.intent && ` · ${commandIntentLabel(command.intent)}`} <small>#{command.id} · {command.actor}</small></p>
    {command.text && <p className="command-ladder-text">{command.text}</p>}
    <ol>
      {steps.map((step, index) => <li key={step.key} data-reached={step.reached ? "true" : "false"} aria-current={index === current ? "step" : undefined}>
        <span>{step.label}</span><time dateTime={step.at ?? undefined} title={step.at ? formatMinuteKST(step.at) || step.at : undefined}>{step.at ? formatMinuteKST(step.at) || step.at : "—"}</time>
      </li>)}
    </ol>
    {command.state === "failed" && <p role="alert" className="command-ladder-error">실패 · {commandErrorLabel(command.last_error_code)}</p>}
    <p className="command-ladder-expiry">기한 {formatMinuteKST(command.expires_at) || command.expires_at}</p>
  </div>;
}
