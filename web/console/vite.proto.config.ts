import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const root = dirname(fileURLToPath(import.meta.url));

// Prototype-only build. Emits to gitignored dist-proto/ and is never wired
// into vite.config.ts rollupOptions.input — the production build's entry
// set (fleet.js, board.js, fleet.css, board.css, shared-*.js) is unchanged.
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
