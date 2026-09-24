// #620 Deploys page — the #529 per-service deploy status block on its own
// screen, off the queue. Read-only: the panel owns its fetch and poll
// against GET /ui/api/deploy-pending.

import { DeployPanel } from "../DeployPanel";

export function DeploysApp() {
  return (
    <div className="dp-root">
      <header className="dp-head">
        <h1>배포 — Deploys</h1>
        <p className="dp-sub">
          서비스별 현재 판과 마지막 배포 이후 머지된 태스크. 읽기 전용이며 기록은 deploy-record/v0 문서에서 옵니다.
        </p>
      </header>
      <DeployPanel />
    </div>
  );
}
