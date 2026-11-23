// Its own file: the mock's factory records whether Mermaid was ever imported,
// and this file's only check is that a document without a mermaid block never
// imports it.
import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import DocMarkdown from "./DocMarkdown";

const loaded = vi.hoisted(() => ({ mermaid: false }));
vi.mock("mermaid", () => {
  loaded.mermaid = true;
  return { default: { initialize: vi.fn(), parse: vi.fn(), render: vi.fn() } };
});

describe("Mermaid lazy loading", () => {
  it("never imports Mermaid for a document without a mermaid block", async () => {
    const body = "# Doc\n\n```ts\nconst x = 1\n```\n\n<!-- file: tasks.md -->\n\nSee [tasks](tasks.md).";
    render(<DocMarkdown body={body} section={null} />);
    expect(await screen.findByRole("heading", { name: "Doc" })).toBeTruthy();
    expect(screen.getByRole("link", { name: "tasks" })).toBeTruthy();
    expect(loaded.mermaid).toBe(false);
  });
});
