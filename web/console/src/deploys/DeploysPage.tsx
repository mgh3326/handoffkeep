import type { DeployPendingResponse } from "./deploy";
import { DeployPanel } from "./DeployPanel";
import "./deploys.css";

export function DeploysPage(props: { fetchStatus?: () => Promise<DeployPendingResponse>; pollMs?: number }) {
  return (
    <div className="dp-root">
      <header className="dp-head">
        <h1>배포</h1>
        <p className="dp-sub">서비스별 현재 판과 마지막 배포 이후 머지된 태스크</p>
      </header>
      <DeployPanel {...props} />
    </div>
  );
}
