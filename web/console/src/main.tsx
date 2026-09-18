import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { FleetApp } from "./FleetApp";
import "./fleet.css";

const root = document.getElementById("fleet-root");
if (root) {
  createRoot(root).render(
    <StrictMode>
      <FleetApp />
    </StrictMode>,
  );
}
