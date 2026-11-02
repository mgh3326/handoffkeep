import { PageShell } from "./shared/AppShell";
import { createRoot } from "react-dom/client";
import { FleetApp } from "./FleetApp";
import "./fleet.css";

const root = document.getElementById("fleet-root");
if (root) {
  createRoot(root).render(
    <PageShell current="fleet">
      <FleetApp />
    </PageShell>,
  );
}
