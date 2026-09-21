import { createRoot } from "react-dom/client";
import { buildDatasets } from "./fixtures";
import { QueueProtoApp } from "./QueueProtoApp";
import "./proto.css";

// Preview-only entry (queue-proto.html via vite.proto.config.ts → gitignored
// dist-proto/). This is the sole fixture path: the production entry
// (main.tsx → LiveQueue) never imports fixtures. ?set=<name> still selects a
// synthetic dataset for the measurement harness.
const root = document.getElementById("queue-proto-root");
if (root) {
  // StrictMode is intentionally off so ?perf=1 timing reflects single
  // renders, matching how the production console entries are measured.
  createRoot(root).render(<QueueProtoApp datasets={buildDatasets()} />);
}
