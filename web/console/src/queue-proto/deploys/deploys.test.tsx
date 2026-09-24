// #620 Deploys page — mounts the #529 panel under the shared shell. The
// panel's own render rules live in ../deploy.test.tsx; here the page owns
// the header copy and the fact that the panel is its content.

import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { DeploysApp } from "./DeploysApp";
import type { DeployPendingResponse } from "../deploy";

function fixture(): DeployPendingResponse {
  return {
    generated_at: "2026-09-23T04:00:00Z",
    pr_source: "refs.pr",
    services: [
      {
        service: "handoffkeep",
        repo: "mgh3326/handoffkeep",
        target: "ncp",
        doc_url: "/ui/doc/deploy/handoffkeep/",
        record_count: 1,
        current: {
          record_key: "deploy/handoffkeep/20260923T033049Z",
          result: "success",
          deployed_ref: "92ee9e2d0ddc47593677adcc9e83040dd9873463",
          deployed_at: "2026-09-22T13:40:36Z",
          failed_step: null,
          recorded_at: "2026-09-23T03:30:49Z",
          source: "backfill",
        },
        merged_boundary: "deployed_at",
        merged_since: [],
      },
    ],
  };
}

describe("DeploysApp (#620)", () => {
  it("renders the page header and the deploy panel with service rows", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(() => Promise.resolve(new Response(JSON.stringify(fixture()), { status: 200 }))),
    );
    render(<DeploysApp />);
    expect(screen.getByRole("heading", { level: 1 }).textContent).toContain("배포");
    await waitFor(() => expect(screen.getByLabelText("배포 상태")).toBeTruthy());
    expect(screen.getByText("handoffkeep")).toBeTruthy();
    expect(screen.getByText("92ee9e2")).toBeTruthy();
    vi.unstubAllGlobals();
  });

  it("a failed first load shows the explicit error, not an empty page", async () => {
    vi.stubGlobal("fetch", vi.fn(() => Promise.reject(new Error("boom"))));
    render(<DeploysApp />);
    await waitFor(() => expect(screen.getByText(/불러오지 못했습니다/)).toBeTruthy());
    vi.unstubAllGlobals();
  });
});
