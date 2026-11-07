import { afterEach, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { App } from "./App";

afterEach(() => {
  vi.unstubAllGlobals();
  window.history.pushState(null, "", "/");
});

it("/ui/tasks/42 fetches and displays the task record", async () => {
  const fetchMock = vi.fn(async () => new Response(JSON.stringify({
    task: { id: 42, title: "Example task", lane: "example", kind: "implement", state: "developing", priority: 2, created_by: "operator", created_at: "2026-11-05T00:00:00Z", updated_at: "2026-11-05T00:00:00Z", refs: {} },
    events: [], dwell: [], linear: null, participants: { task_ref: "hk:task/42", coverage: "not_collected", segments: [] },
    origins: [], documents: [], changes: [], jobs: [], commands: [], commands_truncated: false, origins_truncated: false, documents_truncated: false, changes_truncated: false, jobs_truncated: false,
  }), { status: 200, headers: { "Content-Type": "application/json" } }));
  vi.stubGlobal("fetch", fetchMock);
  window.history.pushState(null, "", "/ui/tasks/42");
  render(<App />);
  expect(await screen.findByRole("heading", { name: "#42 Example task" })).toBeTruthy();
  expect(fetchMock).toHaveBeenCalledWith("/ui/api/board/tasks/42");
});
