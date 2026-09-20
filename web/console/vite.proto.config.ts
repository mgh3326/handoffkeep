import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const root = dirname(fileURLToPath(import.meta.url));

// Standalone preview build for the queue prototype. Emits to gitignored
// dist-proto/ and is used by the evidence harness (capture.mjs,
// assert-rail-pointer.mjs) via `vite preview`. The same entry module is also
// the production `board` input in vite.config.ts — the committed
// board.js/board.css it emits are what /ui/queue actually serves.
export default defineConfig({
  plugins: [react()],
  base: "/",
  build: {
    modulePreload: false,
    sourcemap: false,
    outDir: "dist-proto",
    emptyOutDir: true,
    rollupOptions: {
      input: {
        "queue-proto": resolve(root, "queue-proto.html"),
      },
    },
  },
});
