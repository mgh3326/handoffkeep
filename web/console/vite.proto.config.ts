import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const root = dirname(fileURLToPath(import.meta.url));

// Fixture-preview build. Emits to gitignored dist-proto/ and shares no input
// with vite.config.ts: the production `board` entry builds src/queue-proto/
// main.tsx (live API data), while this build mounts preview.tsx — the only
// entry that imports the synthetic fixtures.
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
