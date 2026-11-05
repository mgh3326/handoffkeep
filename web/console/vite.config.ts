import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const root = dirname(fileURLToPath(import.meta.url));

export default defineConfig({
  plugins: [react()],
  base: "/ui/static/console/",
  build: {
    modulePreload: true,
    sourcemap: false,
    cssCodeSplit: true,
    outDir: "../../internal/ui/static/console",
    // One build emits every console entry. emptyOutDir runs once before the
    // entries are written, so it can never delete a sibling entry's output.
    emptyOutDir: true,
    rollupOptions: {
      input: {
        // /ui, /ui/work, /ui/doc/{key} and /ui/tasks/{id} load the shell: Home,
        // Work and the lazy document viewer behind one router (hk 3140).
        shell: resolve(root, "src/app/main.tsx"),
        // The server-rendered pages (/ui/decisions, /ui/timeline, /ui/compose)
        // mount their htmx content inside the shell chrome.
        server: resolve(root, "src/server.tsx"),
        // /ui/deploys stays live until C4 (hk 3143).
        deploys: resolve(root, "src/deploys/main.tsx"),
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
