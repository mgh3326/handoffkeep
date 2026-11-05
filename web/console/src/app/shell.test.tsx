import { afterEach, describe, expect, it } from "vitest";
import { render, screen, within } from "@testing-library/react";
import { destinations, Shell } from "./Shell";

const EXPECTED = [
  ["Home", "/ui"],
  ["Work", "/ui/work"],
  ["Decisions", "/ui/decisions"],
  ["Deploys", "/ui/deploys"],
];

function setWidth(px: number) {
  Object.defineProperty(window, "innerWidth", { configurable: true, value: px });
  window.dispatchEvent(new Event("resize"));
}

afterEach(() => setWidth(1024));

describe("Shell", () => {
  it("destinations has exactly the four rows", () => {
    expect(destinations.map((d) => [d.label, d.href])).toEqual(EXPECTED);
  });

  it.each([390, 1440])("renders four nav links with their hrefs at %i px", (width) => {
    setWidth(width);
    render(
      <Shell current="work">
        <p>page</p>
      </Shell>,
    );
    const links = within(screen.getByRole("navigation")).getAllByRole("link");
    expect(links.map((a) => [a.textContent, a.getAttribute("href")])).toEqual(EXPECTED);
    expect(links.find((a) => a.getAttribute("aria-current") === "page")?.textContent).toBe("Work");
  });

  it("marks nothing current for a page outside the four destinations", () => {
    render(
      <Shell current={null}>
        <p>page</p>
      </Shell>,
    );
    expect(screen.getAllByRole("link").some((a) => a.getAttribute("aria-current") === "page")).toBe(false);
  });
});
