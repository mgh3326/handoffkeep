import { createRoot } from "react-dom/client";
import { bootTheme } from "../shared/theme";
import { App } from "./App";
import "./shell.css";

bootTheme();

const root = document.getElementById("shell-root");
if (root) {
  createRoot(root).render(<App />);
}
