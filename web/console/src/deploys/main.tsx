import { createRoot } from "react-dom/client";
import { Shell } from "../app/Shell";
import { bootTheme } from "../shared/theme";
import { DeploysApp } from "./DeploysApp";
import "./deploys.css";

// #620 — separate entry: the deploy status screen ships as its own bundle.
// Same shell and token stylesheet as the other console pages.
bootTheme();

const root = document.getElementById("deploys-root");
if (root) {
  createRoot(root).render(
    <Shell current="deploys">
      <DeploysApp />
    </Shell>,
  );
}
