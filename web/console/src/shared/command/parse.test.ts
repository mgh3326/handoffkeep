import { describe, expect, it } from "vitest";
import { commandSuggestions, parseCommand } from "./parse";
import type { CommandChange } from "./grammar";

const lanes = { lanes: ["console-demo", "fleet-status"] };
const parse = (input: string, context = lanes) => parseCommand(input, context);
const kinds = (changes: CommandChange[]) => changes.map(change => change.kind);

describe("command grammar", () => {
  it("reads every attribute in Korean and English", () => {
    expect(parse("상태 검토 중").changes).toEqual([{ kind: "state", state: "reviewing" }]);
    expect(parse("state reviewing").changes).toEqual([{ kind: "state", state: "reviewing" }]);
    expect(parse("우선순위 2").changes).toEqual([{ kind: "priority", value: 2 }]);
    expect(parse("priority P3").changes).toEqual([{ kind: "priority", value: 3 }]);
    expect(parse("우선순위 없음").changes).toEqual([{ kind: "priority", value: 0 }]);
    expect(parse("레인 fleet-status").changes).toEqual([{ kind: "lane", lane: "fleet-status" }]);
    expect(parse("lane console-demo").changes).toEqual([{ kind: "lane", lane: "console-demo" }]);
    expect(parse("보류").changes).toEqual([{ kind: "hold", held: true }]);
    expect(parse("park").changes).toEqual([{ kind: "hold", held: true }]);
    expect(parse("보류해제").changes).toEqual([{ kind: "hold", held: false }]);
    expect(parse("unpark").changes).toEqual([{ kind: "hold", held: false }]);
    expect(parse("머지됨").changes).toEqual([{ kind: "merged", evidence: "" }]);
    expect(parse("merged evidence https://example.test/pr/1").changes).toEqual([{ kind: "merged", evidence: "https://example.test/pr/1" }]);
    expect(parse("드롭 취소 이유 중복 작업").changes).toEqual([{ kind: "drop", closure: "cancelled", duplicateOf: 0, reason: "중복 작업" }]);
    expect(parse("drop obsolete").changes).toEqual([{ kind: "drop", closure: "obsolete", duplicateOf: 0, reason: "" }]);
    expect(parse("드롭 대체 #3312 이유 같은 일").changes).toEqual([{ kind: "drop", closure: "superseded", duplicateOf: 3312, reason: "같은 일" }]);
    expect(parse("코멘트 내일 확인합니다").changes).toEqual([{ kind: "comment", text: "내일 확인합니다" }]);
    expect(parse("comment check tomorrow").changes).toEqual([{ kind: "comment", text: "check tomorrow" }]);
  });

  it("infers bare values from states, priorities and lanes", () => {
    expect(parse("개발중").changes).toEqual([{ kind: "state", state: "developing" }]);
    expect(parse("development").errors[0]?.message).toContain("알 수 없는 값");
    expect(parse("P2").changes).toEqual([{ kind: "priority", value: 2 }]);
    expect(parse("2").changes).toEqual([{ kind: "priority", value: 2 }]);
    expect(parse("console-demo").changes).toEqual([{ kind: "lane", lane: "console-demo" }]);
    expect(parse("개발 중 P1 fleet-status").changes).toEqual([
      { kind: "state", state: "developing" },
      { kind: "priority", value: 1 },
      { kind: "lane", lane: "fleet-status" },
    ]);
  });

  it("parses several changes on one line in order", () => {
    expect(parse("상태 검토 중 우선순위 1 레인 console-demo").changes).toEqual([
      { kind: "state", state: "reviewing" },
      { kind: "priority", value: 1 },
      { kind: "lane", lane: "console-demo" },
    ]);
  });

  it("takes a comment to the end of the line and stops reading commands", () => {
    expect(parse("우선순위 2 코멘트 상태 개발 중").changes).toEqual([
      { kind: "priority", value: 2 },
      { kind: "comment", text: "상태 개발 중" },
    ]);
  });

  it("takes evidence after a merge or a state change", () => {
    expect(parse("머지됨 근거 https://example.test/pr/9").changes).toEqual([{ kind: "merged", evidence: "https://example.test/pr/9" }]);
    expect(parse("상태 완료 근거 https://example.test/result").changes).toEqual([{ kind: "state", state: "completed", evidence: "https://example.test/result" }]);
  });

  it("reports a token that fits two value domains instead of picking one", () => {
    const result = parse("developing", { lanes: ["developing"] });
    expect(result.changes).toEqual([]);
    expect(result.errors[0]?.message).toContain("여러 값으로 해석될 수 있습니다");
    expect(result.errors[0]?.message).toContain("상태·레인");
  });

  it("reports unknown tokens, missing values and duplicates", () => {
    expect(parse("알수없는값").errors[0]?.message).toContain("알 수 없는 값");
    expect(parse("상태").errors[0]?.message).toContain("상태 값이 필요합니다");
    expect(parse("우선순위 9").errors[0]?.message).toContain("1-4");
    expect(parse("레인").errors[0]?.message).toContain("레인 이름이 필요합니다");
    expect(parse("우선순위 1 우선순위 2").errors[0]?.message).toContain("두 번");
    expect(parse("머지됨 머지됨").errors[0]?.message).toContain("두 번");
  });

  it("reports conflicting commands", () => {
    expect(parse("보류 보류해제").errors[0]?.message).toContain("보류와 보류 해제");
    expect(parse("상태 개발 중 머지됨").errors[0]?.message).toContain("상태 변경과 종결");
    expect(parse("보류 보류").errors[0]?.message).toContain("두 번");
  });

  it("reports drop without a closure or a duplicate number", () => {
    expect(parse("드롭").errors[0]?.message).toContain("종료 유형");
    expect(parse("드롭 대체").errors[0]?.message).toContain("대체 작업 번호");
    expect(kinds(parse("드롭 대체").changes)).toEqual([]);
  });

  it("suggests attributes, values and lanes for the current token", () => {
    expect(commandSuggestions("", lanes).items.map(item => item.value)).toContain("상태");
    expect(commandSuggestions("우선", lanes).items.map(item => item.value)).toContain("우선순위");
    expect(commandSuggestions("상태 개", lanes).items.map(item => item.value)).toContain("개발 중");
    expect(commandSuggestions("레인 fleet", lanes).items.map(item => item.value)).toEqual(["fleet-status"]);
    expect(commandSuggestions("드롭 ", lanes).items.map(item => item.value)).toEqual(["취소", "불필요", "대체"]);
    expect(commandSuggestions("코멘트 아무", lanes).items).toEqual([]);
  });
});
