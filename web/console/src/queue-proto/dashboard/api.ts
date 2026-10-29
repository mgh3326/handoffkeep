// Read-only home dashboard client. /ui/api/dashboard is operator-only on the
// server (service principals get 403); a refusal surfaces as HttpError like
// every other console read.

import { HttpError } from "../../board/api";
import type { DashboardResponse } from "./types";

export async function fetchDashboard(): Promise<DashboardResponse> {
  const response = await fetch("/ui/api/dashboard");
  if (!response.ok) {
    throw new HttpError(response.status);
  }
  const body = (await response.json()) as DashboardResponse;
  // A payload missing a section is a failure, never an empty dashboard.
  if (
    typeof body !== "object" ||
    body === null ||
    !Array.isArray(body.quota) ||
    !Array.isArray(body.work) ||
    !Array.isArray(body.prs) ||
    !Array.isArray(body.missing)
  ) {
    throw new Error("dashboard response is missing a section");
  }
  return body;
}
