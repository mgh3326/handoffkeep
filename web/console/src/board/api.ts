import type { BoardDetail, BoardTask, BoardTasksResponse, PolicyResponse } from "./types";

const PAGE_LIMIT = 500;
const MAX_TASKS = 5000;

async function getJSON<T>(path: string): Promise<T> {
  const response = await fetch(path);
  if (!response.ok) {
    throw new Error(`request failed: ${response.status}`);
  }
  return (await response.json()) as T;
}

export type BoardData = {
  tasks: BoardTask[];
  states: string[];
  truncated: boolean;
};

// Walks the after_id cursor until the server reports the set complete. If the
// client-side cap is hit first the board still renders, marked truncated.
export async function fetchBoardTasks(): Promise<BoardData> {
  const tasks: BoardTask[] = [];
  let states: string[] = [];
  let afterID = 0;
  for (;;) {
    const params = new URLSearchParams({ limit: String(PAGE_LIMIT) });
    if (afterID > 0) {
      params.set("after_id", String(afterID));
    }
    const page = await getJSON<BoardTasksResponse>(`/ui/api/board/tasks?${params.toString()}`);
    states = page.states;
    tasks.push(...page.tasks);
    if (!page.truncated) {
      return { tasks, states, truncated: false };
    }
    afterID = page.next_after_id ?? 0;
    if (afterID <= 0 || tasks.length >= MAX_TASKS) {
      return { tasks, states, truncated: true };
    }
  }
}

export function fetchTaskDetail(id: number): Promise<BoardDetail> {
  return getJSON<BoardDetail>(`/ui/api/board/tasks/${id}`);
}

export function fetchPolicyActive(): Promise<PolicyResponse> {
  return getJSON<PolicyResponse>("/ui/api/policy/active");
}
