// Wire types for GET /ui/api/fleet-status: the panewire hub's fleet status
// snapshot (hk doc fleet/status/latest), mirrored from panewire's
// docs/fleet-status.md. An account appears only as the shared quota cache's
// <pool>:<16-hex> fingerprint; the document carries no credential.

export interface FleetStatusHost {
  host: string;
  connected: boolean;
  version?: string;
  accepting: boolean;
  lanes_used: number;
  lanes_cap: number;
}

export interface FleetStatusWindow {
  window: string;
  used_pct: number | null;
  resets_at?: string;
}

export interface FleetStatusQuota {
  host: string;
  pool: string;
  account: string;
  windows: FleetStatusWindow[];
  measured_at: string;
  age_s: number;
  fresh_ttl_s: number;
  fresh: boolean;
  free_state?: string;
}

export interface FleetStatusPlan {
  grade: string;
  t: string;
  seat?: string;
  host?: string;
  pool?: string;
  account?: string;
  tier?: string;
  outcome: string;
}

export interface FleetStatusSnapshot {
  v: number;
  at: string;
  hosts: FleetStatusHost[];
  quota: FleetStatusQuota[];
  plan: FleetStatusPlan[];
}

export interface FleetStatusResponse {
  snapshot: FleetStatusSnapshot | null;
  updated_at?: string;
}
