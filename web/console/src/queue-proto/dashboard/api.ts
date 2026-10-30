// Read-only home dashboard client. /ui/api/dashboard is operator-only on the
// server (service principals get 403); a refusal surfaces as HttpError like
// every other console read.

import { getJSON } from "../../shared/http";
import type { DashboardResponse } from "./types";

export async function fetchDashboard(): Promise<DashboardResponse> {
  const body = await getJSON<DashboardResponse>("/ui/api/dashboard");
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
