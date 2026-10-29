import { createRoot } from "react-dom/client";
import { loadTheme, PageShell } from "../AppShell";
import { DashboardApp } from "./DashboardApp";
import "./dashboard.css";

// hk 2280 — separate entry so the home dashboard ships as its own bundle and
// costs the queue nothing. Same shell and token stylesheet as the other
// console pages.
if (loadTheme() === "light") {
  document.documentElement.dataset.theme = "light";
}

const root = document.getElementById("dashboard-root");
if (root) {
  createRoot(root).render(
    <PageShell current="home">
      <DashboardApp />
    </PageShell>,
  );
}
