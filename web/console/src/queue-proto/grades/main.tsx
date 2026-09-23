import { createRoot } from "react-dom/client";
import { loadTheme, PageShell } from "../AppShell";
import { GradesApp } from "./GradesApp";
import "./grades.css";

// Separate entry: the grade table ships as its own bundle so the queue's
// initial load carries none of it (#597 lazy-load constraint). Same shell
// and token stylesheet as the other console pages.
if (loadTheme() === "light") {
  document.documentElement.dataset.theme = "light";
}

const root = document.getElementById("grades-root");
if (root) {
  createRoot(root).render(
    <PageShell current="grades">
      <GradesApp />
    </PageShell>,
  );
}
