import { createRoot } from "react-dom/client";
import { LiveQueue } from "./live";
import { TaskPage } from "./TaskPage";
import "./proto.css";

// Three hosts can mount this entry: the production /ui/queue template and the
// /ui/tasks/<id> deep link (both board.html → #board-root), and a bare
// preview page (#queue-proto-root). The queue gets the live dataset — the
// synthetic fixture path is only reachable through preview.tsx, which the
// production build never imports. The task page is the same board.html
// mount; the id comes from the path, which the server already shape-checked.
const root = document.getElementById("board-root") ?? document.getElementById("queue-proto-root");
if (root) {
  const taskMatch = /^\/ui\/tasks\/(\d+)$/.exec(window.location.pathname);
  // StrictMode is intentionally off so ?perf=1 timing reflects single
  // renders, matching how the production console entries are measured.
  if (taskMatch) {
    createRoot(root).render(<TaskPage id={Number(taskMatch[1])} />);
  } else {
    createRoot(root).render(<LiveQueue />);
  }
}
