// The one-line command language (hk 3364). Korean and English attribute
// names, bare values inferred from the board payload, and one change per
// field. Parsing never guesses: a token that is unknown or that fits more
// than one value domain is an error the preview shows before anything runs.

import { STATE_LABEL } from "../states";

export type DropClosure = "cancelled" | "obsolete" | "superseded";

export type CommandChange =
  | { kind: "state"; state: string; evidence?: string }
  | { kind: "priority"; value: number }
  | { kind: "lane"; lane: string }
  | { kind: "hold"; held: boolean }
  | { kind: "merged"; evidence: string }
  | { kind: "drop"; closure: DropClosure; duplicateOf: number; reason: string }
  | { kind: "comment"; text: string };

export type CommandError = { message: string; token?: string };

export type ParsedCommand = { changes: CommandChange[]; errors: CommandError[] };

export type CommandContext = {
  /** Every lane the store knows; autocompletion and unknown-lane checks. */
  lanes: string[];
};

/** Korean and English names for the three attributes that take a value.
 * The other commands are words on their own. */
export const VALUED_ATTRIBUTES: Record<string, "state" | "priority" | "lane"> = {
  상태: "state",
  state: "state",
  우선순위: "priority",
  priority: "priority",
  레인: "lane",
  lane: "lane",
};

/** A command that stands on its own; some take an optional clause. */
export const COMMAND_WORDS: Record<string, "hold" | "unpark" | "merged" | "drop" | "comment"> = {
  보류: "hold",
  park: "hold",
  보류해제: "unpark",
  unpark: "unpark",
  머지됨: "merged",
  merged: "merged",
  드롭: "drop",
  drop: "drop",
  코멘트: "comment",
  comment: "comment",
};

export const EVIDENCE_WORDS = new Set(["근거", "evidence"]);
export const REASON_WORDS = new Set(["이유", "reason"]);

export const CLOSURES: Record<string, DropClosure> = {
  취소: "cancelled",
  cancel: "cancelled",
  cancelled: "cancelled",
  canceled: "cancelled",
  불필요: "obsolete",
  obsolete: "obsolete",
  대체: "superseded",
  중복: "superseded",
  superseded: "superseded",
  duplicate: "superseded",
};

export const CLOSURE_LABEL: Record<DropClosure, string> = {
  cancelled: "취소",
  obsolete: "불필요",
  superseded: "대체",
};

/** normalizeWord makes "개발 중" and "개발중" the same lookup key; English is
 * case-insensitive. */
export function normalizeWord(value: string): string {
  return value.trim().toLowerCase().replace(/\s+/g, "");
}

function stateAliases(): Map<string, string> {
  const aliases: Record<string, string> = {
    "분류 대기": "triage",
    분류대기: "triage",
    triage: "triage",
    대기: "backlog",
    backlog: "backlog",
    "착수 전": "unstarted",
    착수전: "unstarted",
    unstarted: "unstarted",
    "조사 중": "researching",
    조사중: "researching",
    researching: "researching",
    "개발 중": "developing",
    개발중: "developing",
    developing: "developing",
    "진행 중": "developing",
    진행중: "developing",
    "검토 중": "reviewing",
    검토중: "reviewing",
    reviewing: "reviewing",
    머지됨: "merged",
    merged: "merged",
    배포됨: "deployed",
    deployed: "deployed",
    완료됨: "completed",
    완료: "completed",
    completed: "completed",
    종료됨: "dropped",
    드롭됨: "dropped",
    dropped: "dropped",
    중복: "duplicate",
    duplicate: "duplicate",
    "보류(보관)": "icebox",
    보류보관: "icebox",
    보류: "icebox",
    보관: "icebox",
    icebox: "icebox",
  };
  // The display labels are the operator's vocabulary; keep them even when a
  // label is added to states.ts without a hand-written alias above.
  for (const [state, label] of Object.entries(STATE_LABEL)) {
    if (!(label in aliases)) aliases[label] = state;
    if (!(state in aliases)) aliases[state] = state;
  }
  const map = new Map<string, string>();
  for (const [word, state] of Object.entries(aliases)) map.set(normalizeWord(word), state);
  return map;
}

const STATES = stateAliases();

/** The canonical state for a word or phrase, or null. */
export function lookupState(value: string): string | null {
  return STATES.get(normalizeWord(value)) ?? null;
}

/** Every state value autocomplete offers, Korean first. */
export function stateChoices(): { value: string; label: string }[] {
  const seen = new Set<string>();
  const out: { value: string; label: string }[] = [];
  for (const state of Object.values(STATE_LABEL)) {
    if (seen.has(state)) continue;
    seen.add(state);
    out.push({ value: state, label: state });
  }
  return out;
}

/** True when the parsed state is one the console can actually write through
 * an existing C1/C2 route. Other states are shown as refusals in preview. */
export const ROUTED_STATES = new Set(["icebox", "unstarted", "merged", "completed"]);

export function parsePriority(value: string): number | null {
  const word = value.trim().toLowerCase();
  if (word === "" || word === "없음" || word === "none") return word === "" ? null : 0;
  const match = /^p?([0-4])$/.exec(word);
  return match ? Number(match[1]) : null;
}

export function priorityLabel(value: number): string {
  return value === 0 ? "없음" : `P${value}`;
}

export function closureLabel(closure: DropClosure): string {
  return CLOSURE_LABEL[closure];
}
