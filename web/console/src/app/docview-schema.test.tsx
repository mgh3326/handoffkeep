import { afterEach, describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import DocView from "./DocView";

const DOC_KEY = "tbls/oci/mysql/brewdial/doc";
const SCHEMA_KEY = "tbls/oci/mysql/brewdial/schema.json";
const SHA_A = "a".repeat(64);
const SHA_B = "b".repeat(64);

type Answer = { status: number; body?: unknown };

// Answers /ui/api/board/doc?key=… by the requested key, so each sibling can be
// present, missing or failing on its own.
function answers(byKey: Record<string, Answer>) {
  const fetchMock = vi.fn(async (url: string) => {
    const key = new URL(url, "http://console.test").searchParams.get("key") ?? "";
    const answer = byKey[key] ?? { status: 404, body: {} };
    return new Response(JSON.stringify(answer.body ?? {}), { status: answer.status });
  });
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

function payload(key: string, sha256: string, extra: Record<string, unknown> = {}) {
  return {
    key,
    kind: "report",
    sha256,
    updated_at: "2026-11-05T01:02:03Z",
    bytes: 2,
    format: "markdown",
    body: "# Heading",
    ...extra,
  };
}

afterEach(() => vi.unstubAllGlobals());

describe("DocView schema.json beside a tbls doc", () => {
  it("links the sibling schema.json and shows both hashes as the same version", async () => {
    answers({
      [DOC_KEY]: { status: 200, body: payload(DOC_KEY, SHA_A) },
      [SCHEMA_KEY]: { status: 200, body: payload(SCHEMA_KEY, SHA_A, { format: "unsupported", reason: "json", body: "{}" }) },
    });
    render(<DocView docKey={DOC_KEY} />);
    expect((await screen.findByRole("link", { name: SCHEMA_KEY })).getAttribute("href")).toBe(`/ui/doc/${SCHEMA_KEY}`);
    expect(screen.getByText(/Same version/)).toBeTruthy();
    expect(screen.getAllByText(SHA_A)).toHaveLength(2);
  });

  it("shows a different version when the two sha256 values differ", async () => {
    answers({
      [DOC_KEY]: { status: 200, body: payload(DOC_KEY, SHA_A) },
      [SCHEMA_KEY]: { status: 200, body: payload(SCHEMA_KEY, SHA_B, { format: "unsupported", reason: "json", body: "{}" }) },
    });
    render(<DocView docKey={DOC_KEY} />);
    expect(await screen.findByText(/Different versions/)).toBeTruthy();
    expect(screen.getByText(SHA_A)).toBeTruthy();
    expect(screen.getByText(SHA_B)).toBeTruthy();
  });

  it("shows no schema link when the sibling is missing", async () => {
    const fetchMock = answers({ [DOC_KEY]: { status: 200, body: payload(DOC_KEY, SHA_A) } });
    render(<DocView docKey={DOC_KEY} />);
    expect(await screen.findByRole("heading", { name: "Heading" })).toBeTruthy();
    expect(screen.queryByText(/Schema:/)).toBeNull();
    expect(screen.queryByRole("note")).toBeNull();
    expect(fetchMock).toHaveBeenCalledWith(`/ui/api/board/doc?key=${encodeURIComponent(SCHEMA_KEY)}`);
  });

  it("says so when the sibling check fails for another reason", async () => {
    answers({
      [DOC_KEY]: { status: 200, body: payload(DOC_KEY, SHA_A) },
      [SCHEMA_KEY]: { status: 500, body: {} },
    });
    render(<DocView docKey={DOC_KEY} />);
    expect((await screen.findByRole("note")).textContent).toMatch(/Could not check schema.json/);
  });

  it("does not look for a sibling on a document whose key does not end in /doc", async () => {
    const fetchMock = answers({ "decision/2026-11-05/example": { status: 200, body: payload("decision/2026-11-05/example", SHA_A) } });
    render(<DocView docKey="decision/2026-11-05/example" />);
    expect(await screen.findByRole("heading", { name: "Heading" })).toBeTruthy();
    expect(fetchMock.mock.calls.some(([url]) => String(url).includes("schema.json"))).toBe(false);
  });
});

describe("DocView JSON download control", () => {
  it("offers a download of a JSON document's current body", async () => {
    answers({
      [SCHEMA_KEY]: { status: 200, body: payload(SCHEMA_KEY, SHA_A, { format: "unsupported", reason: "json", body: '{"a":1}' }) },
    });
    render(<DocView docKey={SCHEMA_KEY} />);
    const link = await screen.findByRole("link", { name: "Download JSON" });
    expect(link.getAttribute("href")).toBe(`/ui/api/board/doc/download?key=${encodeURIComponent(SCHEMA_KEY)}`);
    expect(link.hasAttribute("download")).toBe(true);
  });

  it("offers no download for a document that is not JSON", async () => {
    answers({
      [DOC_KEY]: { status: 200, body: payload(DOC_KEY, SHA_A, { format: "unsupported", reason: "too_large", body: "x" }) },
    });
    render(<DocView docKey={DOC_KEY} />);
    expect(await screen.findByText(/Too large/)).toBeTruthy();
    expect(screen.queryByRole("link", { name: "Download JSON" })).toBeNull();
  });
});
