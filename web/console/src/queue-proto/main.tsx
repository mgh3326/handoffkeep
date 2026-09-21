import { createRoot } from "react-dom/client";
import { LiveQueue } from "./live";
import { TaskPage } from "./TaskPage";
import { loadTheme, PageShell } from "./AppShell";
import "./proto.css";

// The --hk-* tokens arrive as their own stylesheet (/ui/static/console/
// tokens.css, linked by the Go template) so this entry never bundles a copy.
// Dark is the default; light only when the viewer picked it before.
if (loadTheme() === "light") {
  document.documentElement.dataset.theme = "light";
}

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
    createRoot(root).render(
      <PageShell current="queue">
        <TaskPage id={Number(taskMatch[1])} />
      </PageShell>,
    );
  } else {
    createRoot(root).render(<LiveQueue />);
  }
}
