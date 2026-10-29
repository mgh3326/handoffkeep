import { createRoot } from "react-dom/client";
import { loadTheme, PageShell } from "../AppShell";
import { DocsApp } from "./DocsApp";
import "./docs.css";

// hk 2245 — separate entry so document browsing ships as its own bundle and
// costs the queue nothing. Same shell and token stylesheet as the other
// console pages.
if (loadTheme() === "light") {
  document.documentElement.dataset.theme = "light";
}

const root = document.getElementById("docs-root");
if (root) {
  createRoot(root).render(
    <PageShell current="docs">
      <DocsApp />
    </PageShell>,
  );
}
