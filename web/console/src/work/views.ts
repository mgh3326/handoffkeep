import type { BoardTask } from "../shared/boardTypes";

export const views = ["needs-me", "active", "backlog", "shipped", "all"] as const;
export type View = typeof views[number];
const terminal = new Set(["merged", "deployed", "completed", "dropped", "duplicate", "canceled"]);
export function inView(task: BoardTask, view: View): boolean {
  switch (view) {
    case "needs-me": return task.state === "backlog" || task.state === "unstarted" || task.awaiting_decision === true || (!terminal.has(task.state) && (task.refs.decision_request?.status === "open" || task.kind === "decide"));
    case "active": return ["researching", "developing", "reviewing"].includes(task.state);
    case "backlog": return ["triage", "backlog", "unstarted", "icebox"].includes(task.state);
    case "shipped": return ["merged", "deployed", "completed"].includes(task.state);
    case "all": return true;
  }
}
