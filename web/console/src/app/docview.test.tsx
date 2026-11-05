import { afterEach, describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import DocView from "./DocView";

const valid = {
  key: "decision/2026-11-05/example",
  kind: "decision",
  sha256: "0".repeat(64),
  updated_at: "2026-11-05T01:02:03Z",
  bytes: 24,
  format: "markdown",
  body: "# Heading\n\nbody **text**",
};

function answer(status: number, body?: unknown) {
  const fetchMock = vi.fn(async () => new Response(JSON.stringify(body ?? {}), { status }));
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

afterEach(() => vi.unstubAllGlobals());

describe("DocView", () => {
  it("renders a valid document's body and asks for the encoded key", async () => {
    const fetchMock = answer(200, valid);
    render(<DocView docKey={valid.key} />);
    expect(await screen.findByRole("heading", { name: "Heading" })).toBeTruthy();
    expect(screen.getByText("text").tagName).toBe("STRONG");
    expect(fetchMock).toHaveBeenCalledWith("/ui/api/board/doc?key=decision%2F2026-11-05%2Fexample");
  });

  it("never renders raw HTML or a script link from the document body", async () => {
    answer(200, { ...valid, body: "# T\n\n<script>window.__x = 1</script>\n\n[go](javascript:alert(1))" });
    const { container } = render(<DocView docKey={valid.key} />);
    expect(await screen.findByRole("heading", { name: "T" })).toBeTruthy();
    expect(container.querySelector("script")).toBeNull();
    expect(container.querySelector('a[href^="javascript"]')).toBeNull();
  });

  it("shows an unsupported document as plain text with the reason", async () => {
    answer(200, { ...valid, format: "unsupported", reason: "json", body: '{"a":1}' });
    render(<DocView docKey={valid.key} />);
    expect(await screen.findByText('{"a":1}')).toBeTruthy();
    expect(screen.getByText(/JSON document/)).toBeTruthy();
  });

  it.each(["body", "key", "kind", "format", "updated_at", "sha256"])(
    "a payload missing %s shows the error state, not an empty page",
    async (field) => {
      const bad: Record<string, unknown> = { ...valid };
      delete bad[field];
      answer(200, bad);
      render(<DocView docKey={valid.key} />);
      expect((await screen.findByRole("alert")).textContent).toMatch(/Could not load/);
      expect(screen.queryByRole("article")).toBeNull();
    },
  );

  it("a payload with a mistyped field is malformed", async () => {
    answer(200, { ...valid, body: 7 });
    render(<DocView docKey={valid.key} />);
    expect((await screen.findByRole("alert")).textContent).toMatch(/Could not load/);
  });

  it("404 shows 'document not found'", async () => {
    answer(404);
    render(<DocView docKey={valid.key} />);
    expect((await screen.findByRole("alert")).textContent).toMatch(/document not found/i);
  });

  it("any other failure shows the error line", async () => {
    answer(500);
    render(<DocView docKey={valid.key} />);
    expect((await screen.findByRole("alert")).textContent).toMatch(/Could not load this document/);
  });
});
