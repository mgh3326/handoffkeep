// Poll for /ui/api/deploy-pending — the Deploys page panel (#529) fetches the
// payload on a fixed cadence through the console's one poll loop (usePoll).

import { useCallback } from "react";
import { usePoll } from "../shared/usePoll";
import { fetchDeployPending, isDeployPendingResponse, type DeployPendingResponse } from "./deploy";

export function useDeployPending(fetchStatus: () => Promise<DeployPendingResponse> = fetchDeployPending, pollMs = 15_000) {
  // A payload that only matches the outer shape is still a contract
  // violation — validate nested rows, never render a partial board.
  const load = useCallback(async () => {
    const next = await fetchStatus();
    if (!isDeployPendingResponse(next)) {
      throw new Error("malformed deploy-pending payload");
    }
    return next;
  }, [fetchStatus]);
  const { data, failed } = usePoll(load, pollMs);
  return { data, failed };
}
