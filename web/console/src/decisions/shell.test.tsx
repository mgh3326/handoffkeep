import { afterEach, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { Shell } from "../app/Shell";

afterEach(() => vi.unstubAllGlobals());
it("A14 the badge reports overdue as a subset of open", async () => {
  vi.stubGlobal(
    "fetch",
    vi.fn(
      async () =>
        new Response(
          JSON.stringify({
            generated_at: "now",
            decisions: { count: 6, overdue: 2, items: [] },
            tasks: { count: 0, awaiting_decision: 0, stalled: 0, items: [] },
            deploys: { count: 0, events_capped: false, items: [] },
          }),
        ),
    ),
  );
  render(<Shell current="decisions">content</Shell>);
  expect(
    (await screen.findAllByText("열린 결정 6건, 기한 경과 2건")).length,
  ).toBeGreaterThan(0);
  expect(screen.queryByText("8")).toBeNull();
});
it("A17 one polite region announces a result exactly once", async () => {
  vi.stubGlobal(
    "fetch",
    vi.fn(
      async () =>
        new Response(
          JSON.stringify({
            generated_at: "now",
            decisions: { count: 0, overdue: 0, items: [] },
            tasks: { count: 0, awaiting_decision: 0, stalled: 0, items: [] },
            deploys: { count: 0, events_capped: false, items: [] },
          }),
        ),
    ),
  );
  const { container } = render(<Shell current="decisions">content</Shell>);
  await screen.findAllByText("열린 결정 0건");
  window.dispatchEvent(
    new CustomEvent("hk-console-announce", { detail: "접수 · 확인 대기" }),
  );
  const region = container.querySelectorAll('[aria-live="polite"]');
  expect(region).toHaveLength(1);
  await vi.waitFor(() =>
    expect(region[0].textContent).toBe("접수 · 확인 대기"),
  );
});
