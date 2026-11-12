// Path → view for the shell entry. The server renders the same shell page for
// every path below; the browser picks the view from location.pathname.

import { Component, lazy, Suspense, type ReactNode } from "react";
import { DecisionPage } from "../decisions/DecisionPage";
import { DecisionList } from "../decisions/DecisionList";
import { DeploysPage } from "../deploys/DeploysPage";
import type { NavKey } from "./Shell";
import { HomeAttention as Home } from "../home/HomeAttention";
import { Moved } from "./Placeholders";
import { WorkPage } from "../work/WorkPage";
import { TaskRecord } from "../work/TaskRecord";
import { WorkStatusPage } from "../work-status/WorkStatusPage";

// The document viewer pulls in the markdown renderer, so it is a lazy chunk:
// Home and Work never load it.
const DocView = lazy(() => import("./DocView"));

export type Route =
  | { view: "decisions" }
  | { view: "decision"; id: string }
  | { view: "home" }
  | { view: "deploys" }
  | { view: "work"; replaceWith?: string }
  | { view: "work-status" }
  | { view: "doc"; key: string }
  | { view: "task"; id: string }
  | { view: "moved" };

const TASK_PATH = /^\/ui\/tasks\/([1-9][0-9]{0,14})$/;

function docKey(pathname: string): string | null {
  const raw = pathname.slice("/ui/doc/".length);
  try {
    const key = raw.split("/").map(decodeURIComponent).join("/");
    return key === "" ? null : key;
  } catch {
    return null;
  }
}

export function matchRoute(pathname: string): Route {
  const path = pathname.length > 1 ? pathname.replace(/\/+$/, "") : pathname;
  if (path === "/ui" || path === "/ui/home") return { view: "home" };
  if (path === "/ui/decisions") return { view: "decisions" };
  const decision = /^\/ui\/decisions\/([1-9][0-9]{0,14})$/.exec(path);
  if (decision) return { view: "decision", id: decision[1] };
  if (path === "/ui/deploys") return { view: "deploys" };
  if (path === "/ui/work") return { view: "work" };
  if (path === "/ui/work-status") return { view: "work-status" };
  // /ui/queue is the old bookmark for the queue; it now lands on Work.
  if (path === "/ui/queue") return { view: "work", replaceWith: "/ui/work" };
  if (pathname.startsWith("/ui/doc/")) {
    const key = docKey(pathname);
    if (key !== null) return { view: "doc", key };
  }
  const task = TASK_PATH.exec(path);
  if (task) return { view: "task", id: task[1] };
  return { view: "moved" };
}

/** The navigation row a route belongs to; null when it belongs to none. */
export function navKey(route: Route): NavKey | null {
  return route.view === "decision" || route.view === "decisions"
    ? "decisions"
    : route.view === "task"
    ? "work"
    : route.view === "home" || route.view === "work" || route.view === "deploys" || route.view === "work-status"
    ? route.view
    : null;
}

class LoadBoundary extends Component<{ children: ReactNode }, { failed: boolean }> {
  state = { failed: false };
  static getDerivedStateFromError() {
    return { failed: true };
  }
  render() {
    return this.state.failed ? <p role="alert">Could not load this view. Reload the page.</p> : this.props.children;
  }
}

export function RouteView({ route }: { route: Route }) {
  switch (route.view) {
    case "decisions":
      return <DecisionList />;
    case "decision":
      return <DecisionPage id={route.id} />;
    case "home":
      return <Home />;
    case "work":
      return <WorkPage />;
    case "work-status":
      return <WorkStatusPage />;
    case "deploys":
      return <DeploysPage />;
    case "task":
      return <TaskRecord id={Number(route.id)} />;
    case "doc":
      return (
        <LoadBoundary>
          <Suspense fallback={<p role="status">Loading document…</p>}>
            <DocView docKey={route.key} />
          </Suspense>
        </LoadBoundary>
      );
    case "moved":
      return <Moved />;
  }
}
