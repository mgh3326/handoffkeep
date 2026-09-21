import { createRoot } from "react-dom/client";
import { LiveQueue } from "./live";
import "./proto.css";

// Two hosts can mount this entry: the production /ui/queue template
// (board.html → #board-root) and a bare preview page (#queue-proto-root).
// Both get the live dataset — the synthetic fixture path is only reachable
// through preview.tsx, which the production build never imports.
const root = document.getElementById("board-root") ?? document.getElementById("queue-proto-root");
if (root) {
  // StrictMode is intentionally off so ?perf=1 timing reflects single
  // renders, matching how the production console entries are measured.
  createRoot(root).render(<LiveQueue />);
}
