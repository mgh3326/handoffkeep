import { createRoot } from "react-dom/client";
import { QueueProtoApp } from "./QueueProtoApp";
import "./proto.css";

const root = document.getElementById("queue-proto-root");
if (root) {
  // StrictMode is intentionally off so ?perf=1 timing reflects single
  // renders, matching how the production console entries are measured.
  createRoot(root).render(<QueueProtoApp />);
}
