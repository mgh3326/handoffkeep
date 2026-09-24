import { createRoot } from "react-dom/client";
import { loadTheme, PageShell } from "../AppShell";
import { DeploysApp } from "./DeploysApp";
import "./deploys.css";

// #620 — separate entry: the deploy status screen ships as its own bundle so
// the queue's initial load carries none of it (same constraint as #597
// grades). Same shell and token stylesheet as the other console pages.
if (loadTheme() === "light") {
  document.documentElement.dataset.theme = "light";
}

const root = document.getElementById("deploys-root");
if (root) {
  createRoot(root).render(
    <PageShell current="deploys">
      <DeploysApp />
    </PageShell>,
  );
}
