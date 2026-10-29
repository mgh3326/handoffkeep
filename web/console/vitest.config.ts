import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";

// Date formatting in the console is asserted against KST. Set it here, before
// the workers start, so a local run matches CI on any machine.
process.env.TZ = "Asia/Seoul";

export default defineConfig({
  plugins: [react()],
  test: {
    environment: "jsdom",
    setupFiles: ["./src/test/setup.ts"],
  },
});
