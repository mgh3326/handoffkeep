import type { BoardEvent } from "./boardTypes";
import { stateLabel } from "./states";

/** The operator-facing text of one task timeline row. Transition rows label
 * both states; relane and project rows carry names; metadata rows keep their
 * recorded note. */
export function eventText(event: BoardEvent): string {
  if (event.field === "priority") {
    const value = (v?: string) => (v === "0" || !v ? "없음" : `P${v}`);
    const reason = event.note?.replace(/^priority \d+ → \d+(?:: )?/, "");
    return `우선순위 ${value(event.from_value)} → ${value(event.to_value)}${reason ? ` · ${reason}` : ""}`;
  }
  const formatValue = event.kind === "transition" || event.kind === "state_migration" || !event.kind ? stateLabel : (value: string) => value;
  const stateChange = event.from !== event.to ? `${formatValue(event.from)} → ${formatValue(event.to)}` : "";
  const labels: Record<string, string> = { transition: "상태 변경", relane: "레인 변경", decision: "결정", project: "프로젝트 변경", area: "분야 변경", metadata: "정보 변경" };
  const kind = labels[event.kind || "transition"] || "기록";
  return [kind, stateChange, event.note].filter(Boolean).join(" · ");
}
