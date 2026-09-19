import { useRef, useState } from "react";
import { MEASURE_KEY } from "./storage";

// The seven fixed operator scripts (hk:doc 2198 §5). Verbatim-ish: the
// operator compares current-like / compact list / compact board conditions
// over equivalent target sets.
export const EXPERIMENT_SCRIPTS = [
  {
    id: "U1",
    title: "decision discovery",
    text: "find all decision-needed tasks in scope; open the specified one's exact question/evidence",
  },
  {
    id: "U2",
    title: "ID/evidence lookup",
    text: "find task by ID and title fragment; open linked report/PR evidence; search text hidden behind the preview clamp",
  },
  {
    id: "U3",
    title: "longest current wait",
    text: "find it, check evidence; answer `unknown` when cause/due is unknown",
  },
  {
    id: "U4",
    title: "dwell/participation",
    text: "distinguish accumulated dwell vs current wait, and uncollected participation vs 0",
  },
  {
    id: "U5",
    title: "keyboard return",
    text: "switch lane/view, return to previous selection, open next item keyboard-only",
  },
  {
    id: "T6",
    title: "outcome/standalone/unknown",
    text: "find remaining work of one outcome plus standalone/unknown items",
  },
  {
    id: "T7",
    title: "duplicate vs implement/verify",
    text: "distinguish the duplicate-candidate pair from the intentionally similar implement/verify pair; find relation evidence",
  },
  // Iteration-1 trial set (task #462 amendment): five tasks, each run twice,
  // alternating U1-baseline / iter1 order. See evidence/TRIAL.md.
  {
    id: "I1",
    title: "find a view",
    text: "using the view navigator, switch to the All view and report the shown task count",
  },
  {
    id: "I2",
    title: "urgent candidate",
    text: "find the urgent candidate (priority ≥ 90) in the fixture; answer 'none valid' when no task qualifies",
  },
  {
    id: "I3",
    title: "blocked evidence",
    text: "find the blocked task and read its blocker evidence from the detail drawer",
  },
  {
    id: "I4",
    title: "ownership flow",
    text: "trace the claimant chain of the handoff task through its history events",
  },
  {
    id: "I5",
    title: "return to list",
    text: "open a task's detail, close the drawer, and confirm the same row is still selected and in place",
  },
] as const;

export const CONDITIONS = ["current-like", "compact-list", "compact-board", "iter1-rail"] as const;
export const OUTCOMES = ["correct", "wrong-task", "timeout", "abandoned"] as const;

export type MeasureRecord = {
  script: string;
  condition: string;
  outcome: string;
  ms: number;
  at: string;
};

export function loadMeasurements(storage: Pick<Storage, "getItem"> = localStorage): MeasureRecord[] {
  try {
    const raw = storage.getItem(MEASURE_KEY);
    if (!raw) {
      return [];
    }
    const parsed = JSON.parse(raw);
    return Array.isArray(parsed) ? (parsed as MeasureRecord[]) : [];
  } catch {
    return [];
  }
}

export function saveMeasurements(records: MeasureRecord[], storage: Pick<Storage, "setItem"> = localStorage): void {
  try {
    storage.setItem(MEASURE_KEY, JSON.stringify(records));
  } catch {
    // local-only panel — persistence failure just means nothing is stored.
  }
}

export function MeasurePanel() {
  const [records, setRecords] = useState<MeasureRecord[]>(() => loadMeasurements());
  const [script, setScript] = useState<string>(EXPERIMENT_SCRIPTS[0].id);
  const [condition, setCondition] = useState<string>(CONDITIONS[0]);
  const [outcome, setOutcome] = useState<string>(OUTCOMES[0]);
  const [running, setRunning] = useState(false);
  const startRef = useRef(0);

  const start = () => {
    startRef.current = performance.now();
    setRunning(true);
  };

  const stop = () => {
    const ms = Math.round(performance.now() - startRef.current);
    const next = [...records, { script, condition, outcome, ms, at: new Date().toISOString() }];
    setRecords(next);
    saveMeasurements(next);
    setRunning(false);
  };

  const exportJSON = async () => {
    const json = JSON.stringify({ exported_at: new Date().toISOString(), records }, null, 2);
    try {
      await navigator.clipboard.writeText(json);
    } catch {
      // clipboard may be unavailable; fall through to download.
    }
    const blob = new Blob([json], { type: "application/json" });
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = "queue-proto-measurements.json";
    a.click();
    URL.revokeObjectURL(url);
  };

  const active = EXPERIMENT_SCRIPTS.find((s) => s.id === script)!;

  return (
    <details className="qp-measure" id="qp-measure">
      <summary>measurement panel ({records.length} recorded)</summary>
      <p className="muted">
        local-only operator trial recorder — pick script, start/stop, record outcome. Export stays on this machine.
      </p>
      <div className="qp-measure-controls">
        <select aria-label="script" value={script} onChange={(e) => setScript(e.target.value)}>
          {EXPERIMENT_SCRIPTS.map((s) => (
            <option key={s.id} value={s.id}>
              {s.id} {s.title}
            </option>
          ))}
        </select>
        <select aria-label="condition" value={condition} onChange={(e) => setCondition(e.target.value)}>
          {CONDITIONS.map((c) => (
            <option key={c} value={c}>
              {c}
            </option>
          ))}
        </select>
        <select aria-label="outcome" value={outcome} onChange={(e) => setOutcome(e.target.value)}>
          {OUTCOMES.map((o) => (
            <option key={o} value={o}>
              {o}
            </option>
          ))}
        </select>
        {running ? (
          <button type="button" onClick={stop}>
            stop + record
          </button>
        ) : (
          <button type="button" onClick={start}>
            start timer
          </button>
        )}
        <button type="button" onClick={exportJSON}>
          export JSON
        </button>
      </div>
      <p className="qp-measure-script">
        <strong>{active.id}</strong> {active.text}
      </p>
      {records.length > 0 ? (
        <table className="qp-measure-table">
          <thead>
            <tr>
              <th>script</th>
              <th>condition</th>
              <th>outcome</th>
              <th>ms</th>
              <th>at</th>
            </tr>
          </thead>
          <tbody>
            {records.map((r, i) => (
              <tr key={i}>
                <td>{r.script}</td>
                <td>{r.condition}</td>
                <td>{r.outcome}</td>
                <td>{r.ms}</td>
                <td>{r.at}</td>
              </tr>
            ))}
          </tbody>
        </table>
      ) : null}
    </details>
  );
}
