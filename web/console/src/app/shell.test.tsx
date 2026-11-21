import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { destinations, Shell } from "./Shell";
import { STORAGE_KEYS } from "../shared/storage";

// 홈 · 결정 · 작업 · 작업 현황 · 대기 · 배포 · 플릿 현황: a pending decision outranks work, so 결정 is second.
const EXPECTED = [
  ["홈", "/ui"],
  ["결정", "/ui/decisions"],
  ["작업", "/ui/work"],
  ["작업 현황", "/ui/work-status"],
  ["대기", "/ui/waits"],
  ["배포", "/ui/deploys"],
  ["플릿 현황", "/ui/fleet-status"],
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
  it("destinations has exactly the seven rows, in order", () => {
    expect(destinations.map((d) => [d.label, d.href])).toEqual(EXPECTED);
  });

  it.each([390, 1440])("keeps all seven links in the nav, in order, with their hrefs at %i px", (width) => {
    setWidth(width);
    vi.stubGlobal("fetch", vi.fn(async () => new Response("", { status: 503 })));
    render(
      <Shell current="work">
        <p>page</p>
      </Shell>,
    );
    // The 더보기 panel is hidden until opened; the DOM still carries every destination once.
    const links = within(screen.getByRole("navigation")).getAllByRole("link", { hidden: true });
    expect(links.map((a) => [a.textContent, a.getAttribute("href")])).toEqual(EXPECTED);
    expect(links.find((a) => a.getAttribute("aria-current") === "page")?.textContent).toBe("작업");
  });

  it("puts four tabs on the bar and the other three behind 더보기", () => {
    setWidth(320);
    vi.stubGlobal("fetch", vi.fn(async () => new Response("", { status: 503 })));
    render(
      <Shell current="home">
        <p>page</p>
      </Shell>,
    );
    const nav = screen.getByRole("navigation");
    const visible = within(nav).getAllByRole("link").map((a) => a.textContent);
    expect(visible).toEqual(["홈", "결정", "작업", "작업 현황"]);
    const toggle = within(nav).getByRole("button", { name: "더보기" });
    expect(toggle.getAttribute("aria-expanded")).toBe("false");
    expect(within(nav).queryByRole("link", { name: "배포" })).toBeNull();
    expect(within(nav).queryByRole("link", { name: "대기" })).toBeNull();
  });

  it("opens 더보기 on click, closes it on Escape and returns focus to the toggle", () => {
    setWidth(390);
    vi.stubGlobal("fetch", vi.fn(async () => new Response("", { status: 503 })));
    render(
      <Shell current="deploys">
        <p>page</p>
      </Shell>,
    );
    const nav = screen.getByRole("navigation");
    const toggle = within(nav).getByRole("button", { name: "더보기" });
    // The current page is off the bar, so the toggle says so visually.
    expect(toggle.hasAttribute("data-current")).toBe(true);
    fireEvent.click(toggle);
    expect(toggle.getAttribute("aria-expanded")).toBe("true");
    const deploys = within(nav).getByRole("link", { name: "배포" });
    expect(deploys.getAttribute("aria-current")).toBe("page");
    expect(within(nav).getByRole("link", { name: "대기" }).getAttribute("href")).toBe("/ui/waits");
    fireEvent.keyDown(document, { key: "Escape" });
    expect(toggle.getAttribute("aria-expanded")).toBe("false");
    expect(document.activeElement).toBe(toggle);
    expect(within(nav).queryByRole("link", { name: "배포" })).toBeNull();
  });

  it("closes 더보기 on a pointer press outside the nav", () => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response("", { status: 503 })));
    render(
      <Shell current="home">
        <p>page</p>
      </Shell>,
    );
    const toggle = screen.getByRole("button", { name: "더보기" });
    fireEvent.click(toggle);
    expect(toggle.getAttribute("aria-expanded")).toBe("true");
    fireEvent.pointerDown(screen.getByText("page"));
    expect(toggle.getAttribute("aria-expanded")).toBe("false");
  });

  it("marks nothing current for a page outside the seven destinations", () => {
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
    // The other five links carry no counter.
    for (const name of ["홈", "작업", "작업 현황", "배포", "플릿 현황"]) {
      expect(within(screen.getByRole("link", { name, hidden: true })).queryByText(/\d/)).toBeNull();
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
