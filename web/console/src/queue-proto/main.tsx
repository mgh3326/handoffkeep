import { createRoot } from "react-dom/client";
import { QueueProtoApp } from "./QueueProtoApp";
import "./proto.css";

// Two hosts mount this entry: the preview page (queue-proto.html →
// #queue-proto-root) and the production /ui/queue template (board.html →
// #board-root).
const root = document.getElementById("queue-proto-root") ?? document.getElementById("board-root");
if (root) {
  // StrictMode is intentionally off so ?perf=1 timing reflects single
  // renders, matching how the production console entries are measured.
  createRoot(root).render(<QueueProtoApp />);
}
