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
        // /ui/queue loads the queue app built from src/queue-proto: board.js/
        // board.css are what the page actually serves. The entry fetches the
        // real board API — the synthetic fixture path lives only in
        // preview.tsx, which this input never reaches.
        board: resolve(root, "src/queue-proto/main.tsx"),
        // /ui/grades loads the read-only grade table built from src/queue-proto/grades —
        // a separate entry so the queue's initial bundle carries none of it.
        grades: resolve(root, "src/queue-proto/grades/main.tsx"),
        // The --hk-* design tokens ship as their own static stylesheet so the
        // React entries and the Go templates link one and the same file.
        tokens: resolve(root, "src/design/tokens.css"),
      },
      output: {
        entryFileNames: "[name].js",
        chunkFileNames: "shared-[name].js",
        assetFileNames: "[name][extname]",
        // Pin the react/react-dom runtime to a fixed chunk name — the served
        // asset contract (and its cache tests) reference shared-client.js,
        // and a build-derived name would shift whenever module sharing moves.
        manualChunks(id) {
          if (/node_modules\/(react|react-dom|scheduler)\//.test(id)) {
            return "client";
          }
        },
      },
    },
  },
});
