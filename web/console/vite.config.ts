import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const root = dirname(fileURLToPath(import.meta.url));

export default defineConfig({
  plugins: [react()],
  base: "/ui/static/console/",
  build: {
    modulePreload: false,
    sourcemap: false,
    cssCodeSplit: true,
    outDir: "../../internal/ui/static/console",
    // One build emits every console entry. emptyOutDir runs once before the
    // entries are written, so it can never delete a sibling entry's output.
    emptyOutDir: true,
    rollupOptions: {
      input: {
        fleet: resolve(root, "src/main.tsx"),
        // /ui/queue loads the queue prototype: board.js/board.css are built
        // from src/queue-proto/, so the committed bundle is what the page
        // actually serves.
        board: resolve(root, "src/queue-proto/main.tsx"),
      },
      output: {
        entryFileNames: "[name].js",
        chunkFileNames: "shared-[name].js",
        assetFileNames: "[name][extname]",
      },
    },
  },
});
