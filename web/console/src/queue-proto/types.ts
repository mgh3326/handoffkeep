// Synthetic-only prototype model. These shapes mirror the read-only queue
// contract (hk:doc 2199) but every record is generated locally; nothing here
// is a production task.

export const KNOWN_STATES = [
  "backlog",
  "claimed",
  "in_progress",
  "verifying",
  "join",
  "hold",
  "needs_decision",
  "merged",
  "dropped",
] as const;

export type ProtoTask = {
  id: number;
  title: string;
  kind: string;
  state: string;
  lane: string;
  /** null renders as "unknown", never as empty or 0. */
  claimant: string | null;
  priority: number;
  created_at: string;
  /** null → current-state age is unknown (distinct from 0). */
  state_entered_at: string | null;
  /** null → due is unavailable, rendered as unknown. */
  due_at: string | null;
  /** null → blocker is unavailable, rendered as unknown. */
  blocker: string | null;
  created_by: string;
  refs: { pr?: string; head_sha?: string; report_path?: string; job_id?: string };
  events: { id: number; from: string; to: string; by: string; note?: string; at: string }[];
  dwell: { state: string; seconds: number; open: boolean }[];
  /** "collected" may carry an honest 0; "not_collected" renders as unknown. */
  coverage: { status: "collected" | "not_collected"; participants: number | null };
  /** present when the task carries an operator decision question. */
  decision?: { question: string; evidence: string };
};

export type RelationType = "duplicate-candidate" | "implements" | "verifies" | "related";

export type Enrichment = {
  /** synthetic draft area; never inferred from the title. */
  area: string | null;
  /** synthetic draft primary bundle (0..1). */
  bundle: string | null;
  /** intentional standalone — explicitly not part of any bundle. */
  standalone: boolean;
  labels: string[];
  relations: { type: RelationType; otherId: number; note: string }[];
};

export type Dataset = {
  key: string;
  label: string;
  generatedAt: string;
  completeness: "complete" | "partial" | "unknown";
  completenessNote: string;
  tasks: ProtoTask[];
  enrichment: Record<number, Enrichment>;
};

export type ProtoView = "operator" | "active" | "backlog" | "all";
export type Layout = "list" | "board";
export type Density = "compact" | "comfortable";
export type Grouping = "none" | "area";

export type FilterState = {
  query: string;
  lane: string;
  kind: string;
  hiddenStates: string[];
  minPriority: number | null;
};

export type ProtoState = {
  view: ProtoView;
  layout: Layout;
  grouping: Grouping;
  density: Density;
  filters: FilterState;
  hiddenColumns: string[];
  collapsedGroups: string[];
};
