import { afterEach, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { App } from "./App";

afterEach(() => {
  vi.unstubAllGlobals();
  window.history.pushState(null, "", "/");
});

it("/ui/tasks/42 renders Task #42 and makes no fetch call", () => {
  const fetchMock = vi.fn();
  vi.stubGlobal("fetch", fetchMock);
  window.history.pushState(null, "", "/ui/tasks/42");
  render(<App />);
  expect(screen.getByRole("heading", { name: "Task #42" })).toBeTruthy();
  expect(screen.getByText(/arrives with C2 \(hk 3142\)/)).toBeTruthy();
  expect(fetchMock).not.toHaveBeenCalled();
});
