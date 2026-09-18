import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

export default defineConfig({
  plugins: [react()],
  base: "/ui/static/console/",
  build: {
    modulePreload: false,
    sourcemap: false,
    cssCodeSplit: false,
    outDir: "../../internal/ui/static/console",
    emptyOutDir: true,
    rollupOptions: {
      output: {
        entryFileNames: "fleet.js",
        chunkFileNames: "fleet-[name].js",
        assetFileNames: "fleet[extname]",
      },
    },
  },
});
