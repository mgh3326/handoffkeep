// Parser and autocomplete for the one-line command bar (hk 3364 §1).
// Grammar, in order of preference:
//   상태|state <state>          우선순위|priority <n|없음>
//   레인|lane <name>            보류|park   보류해제|unpark
//   머지됨|merged [근거 <url>]  드롭|drop <취소|불필요|대체> [이유 …]
//   코멘트|comment <rest of line>
// A value written without its attribute is inferred from the state labels,
// the priorities and the board's lanes. A token that fits none of them, or
// more than one, is an error — never a guess.

import {
  CLOSURES,
  COMMAND_WORDS,
  EVIDENCE_WORDS,
  REASON_WORDS,
  VALUED_ATTRIBUTES,
  lookupState,
  parsePriority,
  type CommandChange,
  type CommandContext,
  type CommandError,
  type ParsedCommand,
} from "./grammar";

type Token = { text: string; start: number };

function tokenize(input: string): Token[] {
  const tokens: Token[] = [];
  const re = /\S+/g;
  for (let match = re.exec(input); match !== null; match = re.exec(input)) {
    tokens.push({ text: match[0], start: match.index });
  }
  return tokens;
}

function lower(value: string): string {
  return value.toLowerCase();
}

function valuedAttribute(word: string): "state" | "priority" | "lane" | null {
  return VALUED_ATTRIBUTES[word] ?? VALUED_ATTRIBUTES[lower(word)] ?? null;
}

function commandWord(word: string): "hold" | "unpark" | "merged" | "drop" | "comment" | null {
  return COMMAND_WORDS[word] ?? COMMAND_WORDS[lower(word)] ?? null;
}

function isEvidence(word: string): boolean {
  return EVIDENCE_WORDS.has(word) || EVIDENCE_WORDS.has(lower(word));
}

function isReason(word: string): boolean {
  return REASON_WORDS.has(word) || REASON_WORDS.has(lower(word));
}

/** A phrase of up to three tokens that names a state, longest first. */
function readState(tokens: Token[], start: number): { state: string; count: number } | null {
  for (let count = Math.min(3, tokens.length - start); count >= 1; count -= 1) {
    const phrase = tokens.slice(start, start + count).map(token => token.text).join(" ");
    const state = lookupState(phrase);
    if (state) return { state, count };
  }
  return null;
}

function taskRef(word: string): number | null {
  const match = /^#?([1-9][0-9]{0,14})$/.exec(word);
  return match ? Number(match[1]) : null;
}

function rest(tokens: Token[], start: number): string {
  return tokens.slice(start).map(token => token.text).join(" ");
}

function describeMatches(state: boolean, priority: boolean, lane: boolean): string {
  const names = [state ? "상태" : "", priority ? "우선순위" : "", lane ? "레인" : ""].filter(Boolean);
  return names.join("·");
}

export function parseCommand(input: string, context: CommandContext = { lanes: [] }): ParsedCommand {
  const tokens = tokenize(input);
  const changes: CommandChange[] = [];
  const errors: CommandError[] = [];
  const seen = new Set<string>();
  const lanes = new Set(context.lanes);
  let index = 0;

  const once = (kind: string, token: string): boolean => {
    if (!seen.has(kind)) {
      seen.add(kind);
      return true;
    }
    errors.push({ message: "같은 항목을 두 번 지정했습니다.", token });
    return false;
  };

  while (index < tokens.length) {
    const token = tokens[index];
    const attribute = valuedAttribute(token.text);
    if (attribute) {
      if (attribute === "state") {
        const value = readState(tokens, index + 1);
        if (!value) {
          errors.push({ message: "상태 값이 필요합니다.", token: token.text });
          index += 1;
          continue;
        }
        let next = index + 1 + value.count;
        let evidence = "";
        if (tokens[next] && isEvidence(tokens[next].text)) {
          const evidenceToken = tokens[next + 1];
          if (!evidenceToken) {
            errors.push({ message: "상태 근거가 필요합니다.", token: tokens[next].text });
            index = tokens.length;
            continue;
          }
          evidence = evidenceToken.text;
          next += 2;
        }
        if (once("state", token.text)) changes.push(evidence === "" ? { kind: "state", state: value.state } : { kind: "state", state: value.state, evidence });
        index = next;
        continue;
      }
      if (attribute === "priority") {
        const next = tokens[index + 1];
        const priority = next ? parsePriority(next.text) : null;
        if (priority === null) {
          errors.push({ message: "우선순위는 1-4 또는 없음이어야 합니다.", token: next?.text ?? token.text });
          index += next ? 2 : 1;
          continue;
        }
        if (once("priority", token.text)) changes.push({ kind: "priority", value: priority });
        index += 2;
        continue;
      }
      const next = tokens[index + 1];
      if (!next || valuedAttribute(next.text) || commandWord(next.text)) {
        errors.push({ message: "레인 이름이 필요합니다.", token: token.text });
        index += 1;
        continue;
      }
      if (once("lane", token.text)) changes.push({ kind: "lane", lane: next.text });
      index += 2;
      continue;
    }

    const command = commandWord(token.text);
    if (command === "hold" || command === "unpark") {
      if (once(command, token.text)) changes.push(command === "hold" ? { kind: "hold", held: true } : { kind: "hold", held: false });
      index += 1;
      continue;
    }

    if (command === "merged") {
      let evidence = "";
      let next = index + 1;
      if (tokens[next] && isEvidence(tokens[next].text)) {
        const value = tokens[next + 1];
        if (!value) {
          errors.push({ message: "머지 근거가 필요합니다.", token: tokens[next].text });
          index = tokens.length;
          continue;
        }
        evidence = value.text;
        next += 2;
      }
      if (once("merged", token.text)) changes.push({ kind: "merged", evidence });
      index = next;
      continue;
    }

    if (command === "drop") {
      let next = index + 1;
      const closureToken = tokens[next];
      const closure = closureToken ? CLOSURES[closureToken.text] ?? CLOSURES[lower(closureToken.text)] : undefined;
      if (!closure) {
        errors.push({ message: "드롭 종료 유형이 필요합니다 (취소·불필요·대체).", token: closureToken?.text ?? token.text });
        index = closureToken ? next + 1 : next;
        continue;
      }
      next += 1;
      let duplicateOf = 0;
      if (closure === "superseded") {
        const refToken = tokens[next];
        const ref = refToken ? taskRef(refToken.text) : null;
        if (ref === null) {
          errors.push({ message: "대체 작업 번호가 필요합니다.", token: refToken?.text ?? closureToken.text });
        } else {
          duplicateOf = ref;
          next += 1;
        }
      }
      if (tokens[next] && isReason(tokens[next].text)) next += 1;
      const reason = rest(tokens, next);
      next = tokens.length;
      if (once("drop", token.text)) {
        if (closure === "superseded" && duplicateOf === 0) {
          // The parse error above already says why; do not add a half change.
        } else {
          changes.push({ kind: "drop", closure, duplicateOf, reason });
        }
      }
      index = next;
      continue;
    }

    if (command === "comment") {
      const text = index + 1 < tokens.length ? rest(tokens, index + 1) : "";
      if (text === "") {
        errors.push({ message: "코멘트 내용이 필요합니다.", token: token.text });
      } else if (once("comment", token.text)) {
        changes.push({ kind: "comment", text });
      }
      index = tokens.length;
      continue;
    }

    if (isEvidence(token.text) || isReason(token.text)) {
      errors.push({ message: `${token.text}는 머지됨·드롭 뒤에만 쓸 수 있습니다.`, token: token.text });
      index += 1;
      continue;
    }

    // Bare value: a state phrase, a priority or a known lane. More than one
    // reading is ambiguous and is reported instead of picked.
    const state = readState(tokens, index);
    const priority = parsePriority(token.text);
    const lane = lanes.has(token.text);
    const matches = Number(state !== null) + Number(priority !== null) + Number(lane);
    if (matches > 1) {
      errors.push({ message: `여러 값으로 해석될 수 있습니다: ${describeMatches(state !== null, priority !== null, lane)}.`, token: token.text });
      index += state?.count ?? 1;
      continue;
    }
    if (state) {
      if (once("state", token.text)) changes.push({ kind: "state", state: state.state });
      index += state.count;
      continue;
    }
    if (priority !== null) {
      if (once("priority", token.text)) changes.push({ kind: "priority", value: priority });
      index += 1;
      continue;
    }
    if (lane) {
      if (once("lane", token.text)) changes.push({ kind: "lane", lane: token.text });
      index += 1;
      continue;
    }
    errors.push({ message: "알 수 없는 값입니다.", token: token.text });
    index += 1;
  }

  const kinds = new Set(changes.map(change => change.kind));
  if (kinds.has("merged") && kinds.has("drop")) {
    errors.push({ message: "종결 명령은 하나만 쓸 수 있습니다." });
  }
  if (kinds.has("hold") && changes.filter(change => change.kind === "hold").length === 2) {
    errors.push({ message: "보류와 보류 해제를 함께 쓸 수 없습니다." });
  }
  const stateChange = changes.find((change): change is Extract<CommandChange, { kind: "state" }> => change.kind === "state");
  if (stateChange && (kinds.has("merged") || kinds.has("drop"))) {
    errors.push({ message: "상태 변경과 종결 명령을 함께 쓸 수 없습니다." });
  }
  if (stateChange && kinds.has("hold")) {
    errors.push({ message: "상태 변경과 보류 명령을 함께 쓸 수 없습니다." });
  }

  return { changes, errors };
}

export type CommandSuggestion = { value: string; label: string; insert: string };
export type SuggestionRange = { start: number; end: number; items: CommandSuggestion[] };

function suggestion(value: string, kind?: string): CommandSuggestion {
  return { value, label: kind ? `${value} · ${kind}` : value, insert: `${value} ` };
}

function unique(items: CommandSuggestion[]): CommandSuggestion[] {
  const seen = new Set<string>();
  return items.filter(item => {
    if (seen.has(item.value)) return false;
    seen.add(item.value);
    return true;
  });
}

/** Autocomplete for the token under the caret (assumed to be at the end). */
export function commandSuggestions(input: string, context: CommandContext = { lanes: [] }): SuggestionRange {
  const prefixMatch = /(\S*)$/.exec(input);
  const prefix = prefixMatch ? prefixMatch[1] : "";
  const start = input.length - prefix.length;
  const before = input.slice(0, start).trimEnd();
  const previous = before === "" ? "" : before.slice(before.search(/\S*$/));
  const needle = lower(prefix);

  let items: CommandSuggestion[];
  if (valuedAttribute(previous) === "state") {
    items = stateValues();
  } else if (valuedAttribute(previous) === "priority") {
    items = ["없음", "P1", "P2", "P3", "P4"].map(value => suggestion(value, "우선순위"));
  } else if (valuedAttribute(previous) === "lane") {
    items = context.lanes.map(value => suggestion(value, "레인"));
  } else if (commandWord(previous) === "drop") {
    items = ["취소", "불필요", "대체"].map(value => suggestion(value, "종료 유형"));
  } else if (commandWord(previous) === "merged") {
    items = [suggestion("근거", "머지 근거")];
  } else if (commandWord(previous) === "comment") {
    items = [];
  } else {
    items = [
      ...Object.keys(VALUED_ATTRIBUTES).map(value => suggestion(value, "항목")),
      ...Object.keys(COMMAND_WORDS).map(value => suggestion(value, "명령")),
      ...stateValues(),
      ...["없음", "P1", "P2", "P3", "P4"].map(value => suggestion(value, "우선순위")),
      ...context.lanes.map(value => suggestion(value, "레인")),
    ];
  }

  return {
    start,
    end: input.length,
    items: unique(items).filter(item => item.value.toLowerCase().startsWith(needle)).slice(0, 12),
  };
}

function stateValues(): CommandSuggestion[] {
  const values = [
    "분류 대기",
    "대기",
    "착수 전",
    "조사 중",
    "개발 중",
    "검토 중",
    "머지됨",
    "배포됨",
    "완료됨",
    "종료됨",
    "중복",
    "보류(보관)",
  ];
  return values.map(value => suggestion(value, "상태"));
}
