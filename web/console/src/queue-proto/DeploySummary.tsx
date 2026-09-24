// #620 — the queue's one-line deploy pointer. The #529 deploy block moved to
// the Deploys page; this line is all the queue keeps, and only while there is
// something to point at: tasks merged after a service's last deploy, or
// malformed deploy records. Loading, a failed fetch, and a clean zero all
// render nothing — the Deploys page owns the detail and the error surface.
// Styles are inline like DeployPanel's: the queue CSS budget has no headroom.

import type { CSSProperties } from "react";
import { fetchDeployPending, type DeployPendingResponse } from "./deploy";
import { useDeployPending } from "./deployPoll";

const lineStyle: CSSProperties = {
  margin: "0 var(--hk-space-4) var(--hk-space-3)",
  fontSize: "var(--hk-font-meta)",
  lineHeight: "var(--hk-leading-meta)",
  color: "var(--hk-text-secondary)",
};

type Props = {
  fetchStatus?: () => Promise<DeployPendingResponse>;
  pollMs?: number;
};

export function DeploySummary({ fetchStatus = fetchDeployPending, pollMs = 15_000 }: Props) {
  const { data } = useDeployPending(fetchStatus, pollMs);
  if (data === null) {
    return null;
  }
  let pending = 0;
  let invalid = 0;
  for (const svc of data.services) {
    pending += svc.merged_since.length;
    invalid += svc.invalid_count ?? 0;
  }
  if (pending === 0 && invalid === 0) {
    return null;
  }
  const parts: string[] = [];
  if (pending > 0) {
    // events_capped means the merged-events scan was truncated — the count
    // is a lower bound, never the full set.
    parts.push(`배포 대기 머지 ${pending}건${data.events_capped ? " 이상" : ""}`);
  }
  if (invalid > 0) {
    parts.push(`형식 오류 기록 ${invalid}건`);
  }
  return (
    <p style={lineStyle} data-testid="deploy-summary">
      {parts.join(" · ")} <a href="/ui/deploys">→ Deploys</a>
    </p>
  );
}
