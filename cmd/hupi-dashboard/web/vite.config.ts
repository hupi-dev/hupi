/// <reference types="vitest/config" />
import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";

// Local dev loop: `npm run dev` starts Vite's dev server and proxies any
// request to /api (and /auth, for Tier 3's login routes) straight through
// to a real running hupi-dashboard instance, so the frontend can be
// iterated on without rebuilding the Go binary each time. Default target
// matches HUPI_DASHBOARD_LISTEN_ADDR's own default (127.0.0.1:8790);
// override with HUPI_DASHBOARD_DEV_PROXY_TARGET for a different port.
const proxyTarget = process.env.HUPI_DASHBOARD_DEV_PROXY_TARGET || "http://127.0.0.1:8790";

export default defineConfig({
  plugins: [react()],
  server: {
    proxy: {
      "/api": { target: proxyTarget, changeOrigin: false },
      "/auth": { target: proxyTarget, changeOrigin: false },
    },
  },
  build: {
    outDir: "dist",
    emptyOutDir: true,
  },
  test: {
    environment: "jsdom",
    setupFiles: ["./src/test/setup.ts"],
    css: false,
  },
});
