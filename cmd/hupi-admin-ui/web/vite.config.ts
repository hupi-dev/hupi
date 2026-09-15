/// <reference types="vitest/config" />
import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";

// Local dev loop: `npm run dev` starts Vite's dev server and proxies any
// request to /api straight through to a real running hupi-admin-ui
// instance, so the frontend can be iterated on without rebuilding the Go
// binary each time. Default target matches docs/ADMIN_UI.md's default
// HUPI_ADMIN_UI_LISTEN_ADDR (127.0.0.1:8788); override with
// HUPI_ADMIN_UI_DEV_PROXY_TARGET if you run the Go server on a different
// port (see cmd/hupi-admin-ui/web/README.md).
const proxyTarget = process.env.HUPI_ADMIN_UI_DEV_PROXY_TARGET || "http://127.0.0.1:8788";

export default defineConfig({
  plugins: [react()],
  server: {
    proxy: {
      "/api": {
        target: proxyTarget,
        changeOrigin: false,
      },
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
