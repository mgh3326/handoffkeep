import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { destinations, Shell } from "./Shell";
import { STORAGE_KEYS } from "../shared/storage";

// 홈 · 결정 · 작업 · 배포: a pending decision outranks work, so 결정 is second.
const EXPECTED = [
  ["홈", "/ui"],
  ["결정", "/ui/decisions"],
  ["작업", "/ui/work"],
  ["배포", "/ui/deploys"],
];

function setWidth(px: number) {
  Object.defineProperty(window, "innerWidth", { configurable: true, value: px });
  window.dispatchEvent(new Event("resize"));
}

function attention(count: number, overdue: number) {
  return new Response(
    JSON.stringify({
      generated_at: "2026-11-06T00:00:00Z",
      decisions: { count, overdue, items: [] },
      tasks: { count: 0, awaiting_decision: 0, stalled: 0, items: [] },
      deploys: { count: 0, events_capped: false, items: [] },
    }),
  );
}

beforeEach(() => {
  localStorage.clear();
  delete document.documentElement.dataset.theme;
});
afterEach(() => {
  setWidth(1024);
  vi.unstubAllGlobals();
});

describe("Shell navigation", () => {
  it("destinations has exactly the four rows, in order", () => {
    expect(destinations.map((d) => [d.label, d.href])).toEqual(EXPECTED);
  });

  it.each([390, 1440])("renders 홈·결정·작업·배포 in order with their hrefs at %i px", (width) => {
    setWidth(width);
    vi.stubGlobal("fetch", vi.fn(async () => new Response("", { status: 503 })));
    render(
      <Shell current="work">
        <p>page</p>
      </Shell>,
    );
    const links = within(screen.getByRole("navigation")).getAllByRole("link");
    expect(links.map((a) => [a.textContent, a.getAttribute("href")])).toEqual(EXPECTED);
    expect(links.find((a) => a.getAttribute("aria-current") === "page")?.textContent).toBe("작업");
  });

  it("marks nothing current for a page outside the four destinations", () => {
    render(
      <Shell current={null}>
        <p>page</p>
      </Shell>,
    );
    expect(screen.getAllByRole("link").some((a) => a.getAttribute("aria-current") === "page")).toBe(false);
  });

  it("shows the open-decision count as a chip under 결정 with an overdue marker", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => attention(3, 1)));
    render(
      <Shell current="home">
        <p>page</p>
      </Shell>,
    );
    const link = await screen.findByRole("link", { name: /^결정/ });
    const chip = await within(link).findByText("3");
    expect(chip.closest(".hk-chip")?.className).toContain("hk-chip--decision");
    expect(link.textContent).toContain("열린 결정 3건");
    expect(link.textContent).toContain("기한 경과 1건");
    // The other three links carry no counter.
    for (const name of ["홈", "작업", "배포"]) {
      expect(within(screen.getByRole("link", { name })).queryByText(/\d/)).toBeNull();
    }
    // One polite region carries the whole summary.
    await waitFor(() => expect(document.querySelector('[aria-live="polite"]')?.textContent).toBe("열린 결정 3건, 기한 경과 1건"));
  });

  it("shows no counter when no decision is open", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => attention(0, 0)));
    render(
      <Shell current="home">
        <p>page</p>
      </Shell>,
    );
    await waitFor(() => expect(document.querySelector('[aria-live="polite"]')?.textContent).toBe("열린 결정 0건"));
    expect(document.querySelector(".hk-chip")).toBeNull();
  });
});

describe("Shell theme toggle", () => {
  it("switches the theme, relabels itself in Korean and persists the choice", () => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response("", { status: 503 })));
    const { unmount } = render(
      <Shell current="home">
        <p>page</p>
      </Shell>,
    );
    const toGetLight = screen.getByRole("button", { name: "밝은 테마" });
    expect(document.documentElement.dataset.theme).toBe("dark");
    fireEvent.click(toGetLight);
    expect(document.documentElement.dataset.theme).toBe("light");
    expect(localStorage.getItem(STORAGE_KEYS.theme)).toBe("light");
    fireEvent.click(screen.getByRole("button", { name: "어두운 테마" }));
    expect(document.documentElement.dataset.theme).toBe("dark");
    expect(localStorage.getItem(STORAGE_KEYS.theme)).toBe("dark");
    fireEvent.click(screen.getByRole("button", { name: "밝은 테마" }));
    unmount();

    // A new page load starts from the stored choice.
    render(
      <Shell current="home">
        <p>page</p>
      </Shell>,
    );
    expect(document.documentElement.dataset.theme).toBe("light");
    expect(screen.getByRole("button", { name: "어두운 테마" })).toBeTruthy();
  });

  it("is a real button that can take focus, and its icon is decorative", () => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response("", { status: 503 })));
    render(
      <Shell current="home">
        <p>page</p>
      </Shell>,
    );
    const button = screen.getByRole("button", { name: "밝은 테마" });
    button.focus();
    expect(document.activeElement).toBe(button);
    expect(button.querySelector("svg")?.getAttribute("aria-hidden")).toBe("true");
  });
});
