// Shared poll for /ui/api/deploy-pending — the Deploys page panel (#529) and
// the queue's one-line summary (#620) fetch the same payload on the same
// cadence. A failed refresh keeps the last good payload and flags it; the
// next poll is scheduled only after the in-flight load settles, so requests
// never overlap.

import { useEffect, useState } from "react";
import { fetchDeployPending, isDeployPendingResponse, type DeployPendingResponse } from "./deploy";

export function useDeployPending(fetchStatus: () => Promise<DeployPendingResponse> = fetchDeployPending, pollMs = 15_000) {
  const [data, setData] = useState<DeployPendingResponse | null>(null);
  const [failed, setFailed] = useState(false);
  useEffect(() => {
    let cancelled = false;
    let timer = 0;
    const load = async () => {
      try {
        const next = await fetchStatus();
        // A payload that only matches the outer shape is still a contract
        // violation — validate nested rows, never render a partial board.
        if (!cancelled && isDeployPendingResponse(next)) {
          setData(next);
          setFailed(false);
        } else if (!cancelled) {
          setFailed(true);
        }
      } catch {
        if (!cancelled) {
          setFailed(true);
        }
      } finally {
        if (!cancelled) {
          timer = window.setTimeout(() => void load(), pollMs);
        }
      }
    };
    void load();
    return () => {
      cancelled = true;
      window.clearTimeout(timer);
    };
  }, [fetchStatus, pollMs]);
  return { data, failed };
}
