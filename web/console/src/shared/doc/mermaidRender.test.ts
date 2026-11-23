import { beforeEach, describe, expect, it, vi } from "vitest";
import { MERMAID_MAX_TEXT, renderMermaid } from "./mermaidRender";

const mermaid = vi.hoisted(() => ({ initialize: vi.fn(), parse: vi.fn(), render: vi.fn() }));
vi.mock("mermaid", () => ({ default: mermaid }));

beforeEach(() => {
  mermaid.initialize.mockReset();
  mermaid.parse.mockReset().mockResolvedValue(true);
  mermaid.render.mockReset();
});

describe("renderMermaid", () => {
  it("draws with the strict, no-HTML-label configuration and the default size limit", async () => {
    mermaid.render.mockResolvedValue({ svg: "<svg></svg>" });
    await renderMermaid("erDiagram\n  A ||--o{ B : has");
    expect(mermaid.initialize).toHaveBeenCalledWith(
      expect.objectContaining({ securityLevel: "strict", htmlLabels: false, startOnLoad: false, maxTextSize: MERMAID_MAX_TEXT }),
    );
    expect(MERMAID_MAX_TEXT).toBe(50_000);
  });

  it("strips links, foreign objects, scripts and handlers from the SVG it inserts", async () => {
    mermaid.render.mockResolvedValue({
      svg:
        '<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"><g class="node"><a href="https://example.com/x"><text>A</text></a></g>' +
        "<foreignObject><div>html label</div></foreignObject><script>window.__x = 1</script>" +
        '<rect width="10" height="10" onclick="alert(2)"></rect></svg>',
    });
    const svg = await renderMermaid("erDiagram\n  A ||--o{ B : has");
    expect(svg).toContain("<text>A</text>");
    expect(svg).toContain("<rect");
    expect(svg.toLowerCase()).not.toContain("foreignobject");
    expect(svg.toLowerCase()).not.toContain("<script");
    expect(svg.toLowerCase()).not.toContain("<a ");
    expect(svg.toLowerCase()).not.toContain("onload");
    expect(svg.toLowerCase()).not.toContain("onclick");
  });

  it("refuses a source over the limit without calling Mermaid", async () => {
    const source = "erDiagram\n" + "A ||--o{ B : has\n".repeat(Math.ceil(MERMAID_MAX_TEXT / 10));
    await expect(renderMermaid(source)).rejects.toThrow(/limit is 50000/);
    expect(mermaid.parse).not.toHaveBeenCalled();
    expect(mermaid.render).not.toHaveBeenCalled();
  });
});
