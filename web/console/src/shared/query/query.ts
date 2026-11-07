// Query syntax for the Work and Home filters (hk 3364 §3). A query is a
// list of field:value pairs, quoted text phrases and the #미해결/#진행중
// shortcuts. The existing filter controls and the query text are two views of
// the same URL state: parseQuery reads the text, formatQuery writes it back,
// and a value this module does not know is reported instead of dropped.

import type { BoardTask } from "../boardTypes";
import { CHIP_GROUP_ORDER, STATE_GROUP_ORDER, STATE_LABEL, stateLabel, statusGroup } from "../states";
import { lookupState, parsePriority } from "../command/grammar";

export type QuerySort = "default" | "priority" | "updated" | "id" | "state";

export type QueryFilters = {
  text: string;
  state: string;
  lane: string;
  project: string;
  kind: string;
  priority: string;
  grade: string;
  tier: string;
  sort: QuerySort | "";
  /** "needs-me" or "active" from #미해결/#진행중; "" when the shortcut is absent. */
  view: string;
};

export const EMPTY_QUERY: QueryFilters = {
  text: "",
  state: "",
  lane: "",
  project: "",
  kind: "",
  priority: "",
  grade: "",
  tier: "",
  sort: "",
  view: "",
};

export type QueryParse = { filters: QueryFilters; errors: string[] };

type Field = "state" | "lane" | "project" | "kind" | "priority" | "grade" | "tier" | "sort";

const FIELDS: Record<string, Field> = {
  상태: "state",
  state: "state",
  레인: "lane",
  lane: "lane",
  프로젝트: "project",
  project: "project",
  종류: "kind",
  kind: "kind",
  우선순위: "priority",
  priority: "priority",
  등급: "grade",
  grade: "grade",
  티어: "tier",
  tier: "tier",
  정렬: "sort",
  sort: "sort",
};

const SORT_VALUES: Record<string, QuerySort> = {
  기본: "default",
  default: "default",
  우선순위: "priority",
  priority: "priority",
  최근: "updated",
  업데이트: "updated",
  updated: "updated",
  번호: "id",
  id: "id",
  상태: "state",
  state: "state",
};

export const SORT_LABEL: Record<QuerySort, string> = {
  default: "기본",
  priority: "우선순위",
  updated: "최근",
  id: "번호",
  state: "상태",
};

const SHORTCUTS: Record<string, string> = {
  "#미해결": "needs-me",
  "#open": "needs-me",
  "#진행중": "active",
  "#진행": "active",
  "#active": "active",
};

export type QueryContext = {
  lanes: string[];
  projects: string[];
  kinds: string[];
  grades: string[];
  tiers: string[];
};

export const EMPTY_CONTEXT: QueryContext = { lanes: [], projects: [], kinds: [], grades: [], tiers: [] };

function lower(value: string): string {
  return value.toLowerCase();
}

function fieldName(value: string): Field | null {
  return FIELDS[value] ?? FIELDS[lower(value)] ?? null;
}

type Word = { word: string; start: number; end: number };

function skipSpaces(text: string, pos: number): number {
  let index = pos;
  while (index < text.length && /\s/.test(text[index])) index += 1;
  return index;
}

function wordAt(text: string, pos: number): Word | null {
  const start = skipSpaces(text, pos);
  const match = /^\S+/.exec(text.slice(start));
  return match ? { word: match[0], start, end: start + match[0].length } : null;
}

function quotedAt(text: string, pos: number): { value: string; end: number } | null {
  const start = skipSpaces(text, pos);
  if (text[start] !== '"') return null;
  let value = "";
  let index = start + 1;
  while (index < text.length) {
    const char = text[index];
    if (char === "\\" && index + 1 < text.length) {
      value += text[index + 1];
      index += 2;
      continue;
    }
    if (char === '"') return { value, end: index + 1 };
    value += char;
    index += 1;
  }
  return { value, end: text.length };
}

function readValue(text: string, pos: number): { value: string; end: number } {
  const quoted = quotedAt(text, pos);
  if (quoted) return quoted;
  const word = wordAt(text, pos);
  return word ? { value: word.word, end: word.end } : { value: "", end: skipSpaces(text, pos) };
}

/** A state value may be two or three words ("개발 중"); longest first. */
function readStateValue(text: string, pos: number): { state: string; end: number } | null {
  let best: { state: string; end: number } | null = null;
  let cursor = pos;
  const words: string[] = [];
  for (let count = 0; count < 3; count += 1) {
    const word = wordAt(text, cursor);
    if (!word) break;
    words.push(word.word);
    const state = lookupState(words.join(" "));
    if (state) best = { state, end: word.end };
    cursor = word.end;
  }
  return best;
}

function validateKnown(value: string): string | null {
  if (value === "") return "값이 필요합니다.";
  return null;
}

export function parseQuery(input: string): QueryParse {
  const filters: QueryFilters = { ...EMPTY_QUERY };
  const errors: string[] = [];
  const seen = new Set<Field>();
  const text: string[] = [];
  let pos = 0;

  const set = (field: Field, value: string, token: string) => {
    if (seen.has(field)) {
      errors.push(`${token} 항목을 두 번 지정했습니다.`);
      return;
    }
    seen.add(field);
    if (field === "sort") filters.sort = value as QuerySort;
    else filters[field] = value;
  };

  while (pos < input.length) {
    pos = skipSpaces(input, pos);
    if (pos >= input.length) break;

    const fieldMatch = /^([^\s:]+)\s*:\s*/.exec(input.slice(pos));
    const field = fieldMatch ? fieldName(fieldMatch[1]) : null;
    if (field && fieldMatch) {
      const valueStart = pos + fieldMatch[0].length;
      if (field === "state") {
        const value = readStateValue(input, valueStart);
        if (!value) {
          const fallback = wordAt(input, valueStart);
          errors.push(`상태 값을 알 수 없습니다: "${fallback?.word ?? ""}".`);
          pos = fallback ? fallback.end : input.length;
          continue;
        }
        set("state", value.state, "상태");
        pos = value.end;
        continue;
      }
      const value = readValue(input, valueStart);
      if (field === "priority") {
        const priority = parsePriority(value.value);
        if (priority === null) {
          errors.push(`우선순위는 1-4 또는 없음이어야 합니다: "${value.value}".`);
        } else {
          set("priority", String(priority), "우선순위");
        }
        pos = value.end;
        continue;
      }
      if (field === "sort") {
        const sort = SORT_VALUES[value.value] ?? SORT_VALUES[lower(value.value)];
        if (!sort) errors.push(`정렬 값을 알 수 없습니다: "${value.value}".`);
        else set("sort", sort, "정렬");
        pos = value.end;
        continue;
      }
      const problem = validateKnown(value.value);
      if (problem) {
        errors.push(`${fieldMatch[1]} ${problem}`);
      } else {
        set(field, value.value, fieldMatch[1]);
      }
      pos = value.end;
      continue;
    }

    if (fieldMatch) {
      errors.push(`알 수 없는 필터입니다: "${fieldMatch[1]}".`);
      pos = wordAt(input, pos)?.end ?? input.length;
      continue;
    }

    const quoted = quotedAt(input, pos);
    if (quoted) {
      text.push(quoted.value);
      pos = quoted.end;
      continue;
    }
    const word = wordAt(input, pos);
    if (!word) break;
    if (word.word.startsWith("#")) {
      const shortcut = SHORTCUTS[word.word];
      if (shortcut) {
        if (filters.view !== "" && filters.view !== shortcut) errors.push("보기 단축어를 두 번 지정했습니다.");
        filters.view = shortcut;
      } else if (/^#?[0-9]+$/.test(word.word)) {
        text.push(word.word);
      } else {
        errors.push(`알 수 없는 단축어입니다: "${word.word}".`);
      }
      pos = word.end;
      continue;
    }
    text.push(word.word);
    pos = word.end;
  }

  filters.text = text.join(" ");
  return { filters, errors };
}

export function quoteQueryText(value: string): string {
  if (!/[\s"#:]/.test(value)) return value;
  return `"${value.replace(/\\/g, "\\\\").replace(/"/g, '\\"')}"`;
}

export function formatQuery(filters: Partial<QueryFilters>): string {
  const parts: string[] = [];
  if (filters.view === "needs-me") parts.push("#미해결");
  else if (filters.view === "active") parts.push("#진행중");
  if (filters.text) parts.push(quoteQueryText(filters.text));
  if (filters.state) parts.push(`상태:${STATE_LABEL[filters.state] ?? filters.state}`);
  if (filters.lane) parts.push(`레인:${quoteQueryText(filters.lane)}`);
  if (filters.project) parts.push(`프로젝트:${quoteQueryText(filters.project)}`);
  if (filters.kind) parts.push(`종류:${quoteQueryText(filters.kind)}`);
  if (filters.priority) parts.push(`우선순위:${filters.priority === "0" ? "없음" : filters.priority}`);
  if (filters.grade) parts.push(`등급:${quoteQueryText(filters.grade)}`);
  if (filters.tier) parts.push(`티어:${quoteQueryText(filters.tier)}`);
  if (filters.sort && filters.sort !== "default") parts.push(`정렬:${SORT_LABEL[filters.sort]}`);
  return parts.join(" ");
}

/** Every value the page can offer for a field. */
function fieldValues(field: Field, context: QueryContext): string[] {
  switch (field) {
    case "state":
      return Object.keys(STATE_LABEL).filter(state => !["awaiting_decision", "reopened", "claimed", "in_progress", "verifying", "join", "hold", "needs_decision"].includes(state)).map(state => stateLabel(state));
    case "priority":
      return ["없음", "1", "2", "3", "4"];
    case "sort":
      return Object.keys(SORT_LABEL).map(key => SORT_LABEL[key as QuerySort]);
    case "lane":
      return context.lanes;
    case "project":
      return context.projects;
    case "kind":
      return context.kinds;
    case "grade":
      return context.grades;
    case "tier":
      return context.tiers;
  }
}

const FIELD_LIST: { value: string; field: Field }[] = [
  { value: "상태", field: "state" },
  { value: "레인", field: "lane" },
  { value: "프로젝트", field: "project" },
  { value: "종류", field: "kind" },
  { value: "우선순위", field: "priority" },
  { value: "등급", field: "grade" },
  { value: "티어", field: "tier" },
  { value: "정렬", field: "sort" },
];

export type QuerySuggestion = { value: string; label: string; insert: string };
export type QuerySuggestionRange = { start: number; end: number; items: QuerySuggestion[] };

export function querySuggestions(input: string, context: QueryContext = EMPTY_CONTEXT): QuerySuggestionRange {
  const tokenStart = input.lastIndexOf(" ") + 1;
  const token = input.slice(tokenStart);
  const colon = token.indexOf(":");

  let field: Field | null = null;
  let prefix = "";
  let start = tokenStart;

  if (colon >= 0) {
    field = fieldName(token.slice(0, colon));
    const after = token.slice(colon + 1);
    prefix = after.trimStart();
    start = tokenStart + colon + 1 + (after.length - prefix.length);
  } else {
    const before = input.slice(0, tokenStart).trimEnd();
    const previous = before === "" ? "" : before.slice(before.search(/\S*$/));
    if (previous.endsWith(":")) {
      field = fieldName(previous.slice(0, -1));
      prefix = token;
      start = tokenStart;
    } else {
      prefix = token;
    }
  }

  const needle = lower(prefix);
  let items: QuerySuggestion[];
  if (field) {
    items = fieldValues(field, context).map(value => ({ value, label: value, insert: `${value} ` }));
  } else {
    items = [
      ...FIELD_LIST.map(entry => ({ value: `${entry.value}:`, label: `${entry.value}:`, insert: `${entry.value}:` })),
      ...["#미해결", "#진행중"].map(value => ({ value, label: value, insert: `${value} ` })),
    ];
  }
  return {
    start,
    end: input.length,
    items: items.filter(item => lower(item.value).startsWith(needle)).slice(0, 12),
  };
}

export function taskMatchesQuery(task: BoardTask, filters: QueryFilters): boolean {
  if (filters.text) {
    const needle = filters.text.trim().toLowerCase();
    const id = String(task.id);
    const textHit = task.title.toLowerCase().includes(needle) || id === needle.replace(/^#/, "");
    if (!textHit) return false;
  }
  if (filters.state && task.state !== filters.state) return false;
  if (filters.lane && task.lane !== filters.lane) return false;
  if (filters.project && (task.project ?? "") !== filters.project) return false;
  if (filters.kind && task.kind !== filters.kind) return false;
  if (filters.priority && task.priority < Number(filters.priority)) return false;
  if (filters.grade && task.refs.linear?.grade !== filters.grade) return false;
  if (filters.tier && task.refs.linear?.tier !== filters.tier) return false;
  return true;
}

function stateOrder(state: string): number {
  const index = STATE_GROUP_ORDER.indexOf(state as (typeof STATE_GROUP_ORDER)[number]);
  return index < 0 ? STATE_GROUP_ORDER.length : index;
}

export function sortTasks<T extends BoardTask>(tasks: T[], sort: QuerySort): T[] {
  const list = [...tasks];
  switch (sort) {
    case "priority":
      return list.sort((a, b) => (a.priority || Number.POSITIVE_INFINITY) - (b.priority || Number.POSITIVE_INFINITY) || b.updated_at.localeCompare(a.updated_at) || b.id - a.id);
    case "updated":
      return list.sort((a, b) => b.updated_at.localeCompare(a.updated_at) || b.id - a.id);
    case "id":
      return list.sort((a, b) => b.id - a.id);
    case "state":
      return list.sort((a, b) => stateOrder(a.state) - stateOrder(b.state) || (a.priority || Number.POSITIVE_INFINITY) - (b.priority || Number.POSITIVE_INFINITY) || b.id - a.id);
    default:
      return list.sort((a, b) => CHIP_GROUP_ORDER.indexOf(statusGroup(a)) - CHIP_GROUP_ORDER.indexOf(statusGroup(b)) || (a.priority || Number.POSITIVE_INFINITY) - (b.priority || Number.POSITIVE_INFINITY) || b.updated_at.localeCompare(a.updated_at) || b.id - a.id);
  }
}
