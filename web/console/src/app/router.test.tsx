import { afterEach, describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { App } from "./App";
import { matchRoute } from "./routes";

// The doc viewer is a lazy chunk; these tests only need to know it is chosen.
vi.mock("./DocView", () => ({ default: ({ docKey }: { docKey: string }) => <p>doc view for {docKey}</p> }));

function visit(path: string) {
  vi.stubGlobal("matchMedia", () => ({ matches: false, addEventListener: () => {}, removeEventListener: () => {} }));
  window.history.pushState(null, "", path);
  return render(<App />);
}

const emptyAttention = {
  generated_at: "2026-11-05T05:00:00Z",
  decisions: { count: 0, overdue: 0, items: [] },
  tasks: { count: 0, awaiting_decision: 0, stalled: 0, items: [] },
  deploys: { count: 0, events_capped: false, items: [] },
};

afterEach(() => {
  vi.unstubAllGlobals();
  window.history.pushState(null, "", "/");
});

describe("shell router", () => {
  it.each(["/ui", "/ui/home"])("%s renders Home", async (path) => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response(JSON.stringify(emptyAttention))));
    visit(path);
    expect(screen.getByRole("heading", { name: "Waiting on me" })).toBeTruthy();
    expect(await screen.findByText("Nothing is waiting on you.")).toBeTruthy();
  });

  it("/ui/work renders Work and keeps the URL", () => {
    visit("/ui/work");
    expect(screen.getByRole("heading", { name: "작업" })).toBeTruthy();
    expect(window.location.pathname).toBe("/ui/work");
  });

  it("/ui/queue renders Work and replaces the URL with /ui/work", () => {
    const length = window.history.length;
    visit("/ui/queue?view=all");
    expect(screen.getByRole("heading", { name: "작업" })).toBeTruthy();
    expect(window.location.pathname).toBe("/ui/work");
    expect(window.location.search).toBe("");
    expect(window.history.length).toBe(length + 1); // pushState in visit(), none from the replace
  });

  it("/ui/doc/{key} renders the document viewer with the decoded key", async () => {
    visit("/ui/doc/decision/2026-11-05/hk-console-rebuild");
    expect(await screen.findByText("doc view for decision/2026-11-05/hk-console-rebuild")).toBeTruthy();
  });

  it("/ui/tasks/{id} loads the task record", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response("", { status: 404 })));
    visit("/ui/tasks/42");
    expect(await screen.findByText("#42 없음")).toBeTruthy();
  });

  it.each(["/ui/fleet", "/ui/grades", "/ui/docs", "/ui/nope", "/ui/doc/", "/ui/tasks/0", "/ui/tasks/x"])(
    "%s renders the moved state with a link to Home",
    (path) => {
      visit(path);
      expect(screen.getByText(/has moved/)).toBeTruthy();
      expect(screen.getByRole("link", { name: "Go to Home" }).getAttribute("href")).toBe("/ui");
    },
  );

  it("marks the current destination only for Home and Work", () => {
    visit("/ui/work");
    const current = screen.getAllByRole("link").filter((a) => a.getAttribute("aria-current") === "page");
    expect(current.map((a) => a.textContent)).toEqual(["작업"]);
  });
});

describe("matchRoute", () => {
  it("tolerates a trailing slash and rejects a malformed escape in a doc key", () => {
    expect(matchRoute("/ui/work/")).toEqual({ view: "work" });
    expect(matchRoute("/ui/doc/%E0%A4%A")).toEqual({ view: "moved" });
  });
});
