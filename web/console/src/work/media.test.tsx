import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { BoardAttachment, BoardTask } from "../shared/boardTypes";
import { MAX_INLINE_BYTES } from "../shared/media";
import { TaskAttachments } from "./TaskAttachments";
import { TaskRecord } from "./TaskRecord";

const task: BoardTask = { id: 101, title: "Example task", lane: "x", kind: "implement", state: "developing", priority: 2, created_by: "operator", created_at: "2026-11-05T00:00:00Z", updated_at: "2026-11-05T01:00:00Z", refs: {}, project: "a" };
const detail = { task, events: [], dwell: [], participants: { task_ref: "hk:task/101", coverage: "not_collected", segments: [] }, origins: [], documents: [], documents_truncated: false, changes: [], jobs: [], origins_truncated: false, changes_truncated: false, jobs_truncated: false };
const json = (data: unknown) => new Response(JSON.stringify(data), { status: 200, headers: { "Content-Type": "application/json" } });

const sha = (c: string) => c.repeat(64);
function attachment(name: string, mime: string, size = 1200, digest = name, archiveLocation = ""): BoardAttachment {
  return { sha256: sha(digest), original_name: name, size_bytes: size, mime, created_at: "2026-11-05T00:00:00Z", created_by: "operator", archived_at: archiveLocation ? "2026-11-16T00:00:00Z" : null, archive_location: archiveLocation };
}
function stubRecord(attachments: BoardAttachment[], extra: { body?: string; comments?: unknown[] } = {}) {
  vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL) => {
    const url = String(input);
    if (url.includes("/attachments")) return json({ attachments, truncated: false });
    if (url.includes("/comments")) return json({ comments: extra.comments ?? [], truncated: false });
    return json({ ...detail, body: extra.body });
  }));
}
afterEach(() => { vi.unstubAllGlobals(); });

describe("task attachments in place", () => {
  it("shows an image inline with a lazy thumbnail and a video with controls", async () => {
    stubRecord([attachment("card-a.png", "image/png", 1200, "a"), attachment("run.mp4", "video/mp4", 2400, "b")]);
    render(<TaskRecord id={101} />);
    const image = await screen.findByRole("img", { name: "card-a.png" });
    expect(image.getAttribute("src")).toBe(`/ui/files/${sha("a")}?inline=1`);
    expect(image.getAttribute("loading")).toBe("lazy");
    const video = screen.getByLabelText("run.mp4");
    expect(video.tagName).toBe("VIDEO");
    expect(video.hasAttribute("controls")).toBe(true);
    expect(video.getAttribute("preload")).toBe("metadata");
    expect(video.getAttribute("src")).toBe(`/ui/files/${sha("b")}?inline=1`);
  });

  it("keeps other files as download links", async () => {
    stubRecord([attachment("plan.pdf", "application/pdf", 900, "c")]);
    render(<TaskRecord id={101} />);
    const link = await screen.findByRole("link", { name: "plan.pdf" });
    expect(link.getAttribute("href")).toBe(`/ui/files/${sha("c")}`);
    expect(screen.queryByRole("img")).toBeNull();
  });

  it("names where an archived file is and does not inline it", async () => {
    stubRecord([attachment("card-b.png", "image/png", 1200, "e", "fleet-artifacts/archive/tasks/7/attachments/e-card-b.png")]);
    render(<TaskRecord id={101} />);
    const link = await screen.findByRole("link", { name: "card-b.png" });
    expect(link.getAttribute("href")).toBe(`/ui/files/${sha("e")}`);
    const line = await screen.findByText(/fleet-artifacts\/archive\/tasks\/7/);
    expect(line.textContent).toContain("보관됨");
    expect(screen.queryByRole("img")).toBeNull();
  });

  it("shows only the link and its size above the inline threshold", async () => {
    stubRecord([attachment("huge.png", "image/png", MAX_INLINE_BYTES + 1, "d")]);
    render(<TaskRecord id={101} />);
    expect(await screen.findByRole("link", { name: "huge.png" })).toBeTruthy();
    expect(screen.getByText(new RegExp(`${MAX_INLINE_BYTES + 1} bytes$`))).toBeTruthy();
    expect(screen.queryByText(/image\/png/)).toBeNull();
    expect(screen.queryByRole("img")).toBeNull();
    expect(screen.queryByText(/두 개 이상 고르면/)).toBeNull();
  });

  it("opens the compare view once two items are chosen", async () => {
    stubRecord([attachment("card-a.png", "image/png", 1200, "a"), attachment("card-b.png", "image/png", 1200, "b"), attachment("card-c.png", "image/png", 1200, "c")]);
    render(<TaskRecord id={101} />);
    const picks = await screen.findAllByRole("button", { name: "비교 선택" });
    fireEvent.click(picks[0]);
    expect(screen.queryByRole("region", { name: "첨부 비교" })).toBeNull();
    fireEvent.click(picks[1]);
    const compare = await screen.findByRole("region", { name: "첨부 비교" });
    expect(within(compare).getByText("card-a.png")).toBeTruthy();
    expect(within(compare).getByText("card-b.png")).toBeTruthy();
    expect(within(compare).queryByText("card-c.png")).toBeNull();
    expect(within(compare).getAllByRole("img")).toHaveLength(2);
  });

  it("opens the enlarge view from a grid thumbnail and steps over every grid item", async () => {
    const files = [attachment("card-a.png", "image/png", 1200, "a"), attachment("card-b.png", "image/png", 1200, "b"), attachment("card-c.png", "image/png", 1200, "c")];
    render(<TaskAttachments attachments={files} />);
    const grid = screen.getByRole("list", { name: "이미지·동영상 첨부" });
    const thumbnail = within(grid).getByRole("button", { name: "card-b.png 크게 보기" });
    fireEvent.click(thumbnail);
    const dialog = await screen.findByRole("dialog", { name: "card-b.png 크게 보기" });
    expect(within(dialog).getByText(/card-b\.png · 2\/3/)).toBeTruthy();
    fireEvent.keyDown(window, { key: "ArrowRight" });
    await waitFor(() => expect(screen.getByRole("dialog", { name: "card-c.png 크게 보기" })).toBeTruthy());
    expect(within(screen.getByRole("dialog")).getByText(/card-c\.png · 3\/3/)).toBeTruthy();
    fireEvent.keyDown(window, { key: "ArrowRight" });
    await waitFor(() => expect(screen.getByRole("dialog", { name: "card-a.png 크게 보기" })).toBeTruthy());
    expect(within(screen.getByRole("dialog")).getByText(/card-a\.png · 1\/3/)).toBeTruthy();
    fireEvent.keyDown(window, { key: "Escape" });
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(document.activeElement).toBe(thumbnail);
  });

  it("gives a video its native controls and a visible enlarge button", async () => {
    render(<TaskAttachments attachments={[attachment("run.mp4", "video/mp4", 2400, "b")]} />);
    const video = screen.getByLabelText("run.mp4");
    expect(video.hasAttribute("controls")).toBe(true);
    const grid = screen.getByRole("list", { name: "이미지·동영상 첨부" });
    fireEvent.click(within(grid).getByRole("button", { name: "run.mp4 크게 보기" }));
    const dialog = await screen.findByRole("dialog", { name: "run.mp4 크게 보기" });
    expect(within(dialog).getByLabelText("run.mp4").tagName).toBe("VIDEO");
  });

  it("enlarges from the compare view and steps over the chosen items only", async () => {
    const files = [attachment("card-a.png", "image/png", 1200, "a"), attachment("card-b.png", "image/png", 1200, "b"), attachment("card-c.png", "image/png", 1200, "c")];
    render(<TaskAttachments attachments={files} />);
    const picks = screen.getAllByRole("button", { name: "비교 선택" });
    fireEvent.click(picks[0]);
    fireEvent.click(picks[2]);
    const compare = await screen.findByRole("region", { name: "첨부 비교" });
    fireEvent.click(within(compare).getByRole("button", { name: "card-a.png 크게 보기" }));
    const dialog = await screen.findByRole("dialog", { name: "card-a.png 크게 보기" });
    expect(within(dialog).getByText(/card-a\.png · 1\/2/)).toBeTruthy();
    fireEvent.keyDown(window, { key: "ArrowRight" });
    await waitFor(() => expect(screen.getByRole("dialog", { name: "card-c.png 크게 보기" })).toBeTruthy());
    expect(within(screen.getByRole("dialog")).getByText(/card-c\.png · 2\/2/)).toBeTruthy();
    fireEvent.keyDown(window, { key: "ArrowRight" });
    await waitFor(() => expect(screen.getByRole("dialog", { name: "card-a.png 크게 보기" })).toBeTruthy());
    expect(within(screen.getByRole("dialog")).queryByText(/card-b\.png/)).toBeNull();
    fireEvent.keyDown(window, { key: "Escape" });
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  });

  it("leaves typing targets alone while enlarged", async () => {
    render(<TaskAttachments attachments={[attachment("card-a.png", "image/png", 1200, "a")]} />);
    const grid = screen.getByRole("list", { name: "이미지·동영상 첨부" });
    fireEvent.click(within(grid).getByRole("button", { name: "card-a.png 크게 보기" }));
    expect(await screen.findByRole("dialog")).toBeTruthy();
    const input = document.createElement("input");
    document.body.appendChild(input);
    fireEvent.keyDown(input, { key: "Escape" });
    expect(screen.getByRole("dialog")).toBeTruthy();
    input.remove();
  });
});

describe("markdown bodies", () => {
  it("renders the task body's heading and list, and keeps the raw text behind 원문", async () => {
    stubRecord([], { body: "## 질문\n\n- 2A 카드\n- 2B 카드\n\n**2단계**를 고릅니다" });
    render(<TaskRecord id={101} />);
    expect(await screen.findByRole("heading", { name: "질문" })).toBeTruthy();
    expect(screen.getAllByRole("listitem").map(item => item.textContent)).toEqual(["2A 카드", "2B 카드"]);
    expect(screen.getByText("2단계").tagName).toBe("STRONG");
    fireEvent.click(screen.getByRole("button", { name: "원문" }));
    expect(screen.queryByRole("heading", { name: "질문" })).toBeNull();
    expect(screen.getByText(/## 질문/).tagName).toBe("PRE");
  });

  it("renders a comment body as markdown with its author above it", async () => {
    stubRecord([], { comments: [{ id: 2, task_id: 101, body: "## 판단\n\n- 하나", author: "worker", created_at: "2026-11-05T02:00:00Z" }] });
    render(<TaskRecord id={101} />);
    expect(await screen.findByRole("heading", { name: "판단" })).toBeTruthy();
    expect(screen.getByText(/· worker$/)).toBeTruthy();
  });
});
