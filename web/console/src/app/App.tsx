import { useEffect, useState } from "react";
import { matchRoute, navKey, RouteView } from "./routes";
import { ActionNotices } from "../shared/actions/TaskActions";
import { Shell } from "./Shell";

export function App() {
  const [route] = useState(() => matchRoute(window.location.pathname));
  useEffect(() => {
    if (route.view === "work" && route.replaceWith) {
      window.history.replaceState(null, "", route.replaceWith);
    }
  }, [route]);
  return (
    <Shell current={navKey(route)}>
      <RouteView route={route} />
      <ActionNotices />
    </Shell>
  );
}
