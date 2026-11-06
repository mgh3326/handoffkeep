import { describe, expect, it } from "vitest";
import { formatAge, formatRemaining, summarizeQuestion } from "./format";

describe("summarizeQuestion", () => {
  it.each([
    ["Ship it? Yes or no.", "Ship it?"],
    ["배포를 진행할까요? 롤백 계획은 아래에.", "배포를 진행할까요?"],
    ["이번 릴리스는 v1.2 입니다. 다음 줄", "이번 릴리스는 v1.2 입니다."],
    ["Deploy now: the plan follows", "Deploy now"],
    ["첫 줄\n둘째 줄", "첫 줄"],
    ["\n\n  ## 제목 **굵게** `코드`\n본문", "제목 굵게 코드"],
    ["- [링크](https://example.test/x) 확인", "링크 확인"],
    [": 콜론으로 시작", ": 콜론으로 시작"],
    ["", ""],
    ["[decision-needed] 결제 재시도 간격을 늘려도 될까요?", "결제 재시도 간격을 늘려도 될까요?"],
    ["[a][b] 두 표식 뒤의 질문입니다.", "두 표식 뒤의 질문입니다."],
    ["[decision-needed]", "[decision-needed]"],
  ])("%j -> %j", (text, want) => {
    expect(summarizeQuestion(text)).toBe(want);
  });

  it("cuts at 90 characters with an ellipsis, counting Hangul as one each", () => {
    const text = "가".repeat(95);
    expect(summarizeQuestion(text)).toBe(`${"가".repeat(90)}…`);
    expect(summarizeQuestion("가".repeat(90))).toBe("가".repeat(90));
  });
});

describe("formatAge and formatRemaining", () => {
  const now = "2026-11-06T12:00:00Z";
  it.each([
    ["2026-11-06T11:59:40Z", "방금"],
    ["2026-11-06T11:48:00Z", "12분"],
    ["2026-11-06T09:00:00Z", "3시간"],
    ["2026-11-04T12:00:00Z", "2일"],
  ])("age of %s is %s", (iso, want) => {
    expect(formatAge(now, iso)).toBe(want);
  });
  it("is null when a time cannot be read", () => {
    expect(formatAge("now", "2026-11-06T09:00:00Z")).toBeNull();
    expect(formatAge(now, "later")).toBeNull();
  });
  it.each([
    ["2026-11-06T12:30:00Z", "30분"],
    ["2026-11-06T17:00:00Z", "5시간"],
    ["2026-11-08T12:00:00Z", "2일"],
    ["2026-11-06T11:00:00Z", null],
  ])("remaining until %s is %s", (iso, want) => {
    expect(formatRemaining(now, iso)).toBe(want);
  });
});
