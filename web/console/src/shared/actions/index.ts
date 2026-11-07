import type { TaskRefs } from "../boardTypes";
import { commandActions } from "./commands";
import { lifecycleActions } from "./lifecycle";

export type ActionTarget = {
  id: number;
  state: string;
  priority: number;
  active_job?: unknown;
  kind?: string;
  lane?: string;
  title?: string;
  awaiting_decision?: boolean;
  refs?: TaskRefs;
};
export type Action = {
  id: string; label: string; keywords: string[]; shortcut?: string;
  kind: "local" | "command"; confirm: "none" | "undo" | "dialog" | "typed";
  /** Which surface offers the action. Absent means a task row. */
  target?: "task" | "deploy";
  applicable: (target: ActionTarget) => boolean;
  disabledReason: (target: ActionTarget) => string | null;
};
const settled = new Set(["merged", "deployed", "completed", "dropped", "duplicate", "canceled"]);
export const actions: Action[] = [{
  id: "priority", label: "우선순위 변경", keywords: ["priority", "우선순위"], shortcut: "p", kind: "local", confirm: "undo",
  applicable: () => true,
  disabledReason: target => settled.has(target.state) ? "종결된 작업은 변경할 수 없습니다" : null,
}, ...lifecycleActions, ...commandActions];
