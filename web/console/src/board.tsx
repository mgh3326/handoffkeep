import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { BoardApp } from "./board/BoardApp";
import "./board.css";

const root = document.getElementById("board-root");
if (root) {
  createRoot(root).render(
    <StrictMode>
      <BoardApp />
    </StrictMode>,
  );
}
