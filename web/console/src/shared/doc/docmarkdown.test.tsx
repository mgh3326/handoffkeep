import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import DocMarkdown from "./DocMarkdown";

const mermaid = vi.hoisted(() => ({ initialize: vi.fn(), parse: vi.fn(), render: vi.fn() }));
vi.mock("mermaid", () => ({ default: mermaid }));

const erd = "```mermaid\nerDiagram\n  A ||--o{ B : has\n```";

beforeEach(() => {
  mermaid.initialize.mockReset();
  mermaid.parse.mockReset().mockResolvedValue(true);
  mermaid.render.mockReset().mockResolvedValue({ svg: '<svg data-test="diagram"><text>A</text></svg>' });
});

describe("DocMarkdown table links", () => {
  const combined = [
    "<!-- file: README.md -->",
    "",
    "# Schema",
    "",
    "<!-- file: tasks.md -->",
    "",
    "## tasks",
    "",
    "See [decisions](decisions.md#columns), [missing](missing.md) and [site](https://example.com/).",
    "",
    "<!-- file: decisions.md -->",
    "",
    "## decisions",
  ].join("\n");

  it("makes a link to a file in the document a fragment to that file's anchor", () => {
    render(<DocMarkdown body={combined} section={null} />);
    const link = screen.getByRole("link", { name: "decisions" });
    expect(link.getAttribute("href")).toBe("#user-content-file-decisions.md");
    expect(document.getElementById("user-content-file-decisions.md")).not.toBeNull();
    expect(document.getElementById("user-content-file-tasks.md")).not.toBeNull();
  });

  it("renders a link to a file the document does not hold as text", () => {
    render(<DocMarkdown body={combined} section={null} />);
    expect(screen.queryByRole("link", { name: "missing" })).toBeNull();
    expect(screen.getByText("missing")).toBeTruthy();
  });

  it("keeps an external https link as it was", () => {
    render(<DocMarkdown body={combined} section={null} />);
    expect(screen.getByRole("link", { name: "site" }).getAttribute("href")).toBe("https://example.com/");
  });
});

describe("DocMarkdown Mermaid blocks", () => {
  it("draws an erDiagram block as SVG with the strict configuration", async () => {
    const { container } = render(<DocMarkdown body={erd} section={null} />);
    await waitFor(() => expect(container.querySelector('img[data-doc-diagram="svg"]')).not.toBeNull());
    const src = container.querySelector("img")?.getAttribute("src") ?? "";
    expect(src.startsWith("data:image/svg+xml;charset=utf-8,")).toBe(true);
    expect(decodeURIComponent(src.slice(src.indexOf(",") + 1))).toContain('<svg data-test="diagram">');
    expect(mermaid.render.mock.calls[0][0]).toMatch(/^hk-mermaid-/);
    expect(mermaid.render.mock.calls[0][1]).toBe("erDiagram\n  A ||--o{ B : has");
    expect(mermaid.initialize).toHaveBeenCalledWith(expect.objectContaining({ securityLevel: "strict", htmlLabels: false }));
    expect(container.querySelector("code.language-mermaid")).toBeNull();
  });

  it("shows the source and the first error line when a block does not parse", async () => {
    mermaid.parse.mockRejectedValue(new Error("Parse error on line 2:\n  A ||--o{ \nExpecting 'ALPHANUM'"));
    const { container } = render(<DocMarkdown body={erd} section={null} />);
    expect((await screen.findByRole("note")).textContent).toBe("The diagram could not be drawn: Parse error on line 2:");
    expect(container.querySelector('pre[data-doc-diagram="failed"]')?.textContent).toBe("erDiagram\n  A ||--o{ B : has");
    expect(container.querySelector("img")).toBeNull();
  });

  it("refuses a block over the size limit with the reason, and never draws it", async () => {
    const source = "erDiagram\n" + "A ||--o{ B : has\n".repeat(5000);
    render(<DocMarkdown body={"```mermaid\n" + source + "\n```"} section={null} />);
    expect((await screen.findByRole("note")).textContent).toMatch(/limit is 50000/);
    expect(mermaid.parse).not.toHaveBeenCalled();
    expect(mermaid.render).not.toHaveBeenCalled();
  });
});
