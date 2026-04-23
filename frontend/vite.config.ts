/// <reference types="vitest" />
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// Vite config for the tenant console. The console API and its
// /api/v1/auth/* routes live on a DEDICATED HTTP server bound to
// cfg.Console.ListenAddr (see cmd/gateway/main.go#startConsoleAPI),
// which is NOT the same listener as the S3 data plane on :8080. The
// canonical console port is 8081; proxy every /api request there so
// the SPA's same-origin fetch hits the console mux instead of the
// S3 mux (which 404s on /api/*).
//
// `process` is available because Vite evaluates this config in a
// Node ESM context. `import.meta.env` is NOT populated at config
// evaluation time (Vite's define-replacement only targets app code),
// so operators who need to point at a staging console must export
// VITE_CONSOLE_URL in the shell before running `vite` / `vite build`
// / `vite preview`. See vitejs/vite#15088 for the upstream note.
declare const process: { env: Record<string, string | undefined> };
const CONSOLE_TARGET = process.env.VITE_CONSOLE_URL || "http://localhost:8081";

export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      "/api": {
        target: CONSOLE_TARGET,
        changeOrigin: true,
      },
    },
  },
  // `vite preview` is a static file server by default — it does NOT
  // inherit the dev-server proxy, so a production-preview build at
  // :4173 responds 405 to POST /api/v1/auth/login because the static
  // server does not know about auth routes. Mirror the dev proxy on
  // preview so the Playwright e2e suite (which boots the preview
  // build; see playwright.config.ts) and operators running the same
  // bundle locally both reach the gateway at :8080.
  preview: {
    port: 4173,
    proxy: {
      "/api": {
        target: CONSOLE_TARGET,
        changeOrigin: true,
      },
    },
  },
  test: {
    // The Playwright e2e scaffold under tests/e2e/ uses a different
    // runner (playwright test) and imports from @playwright/test,
    // which is not compatible with the vitest test API. Exclude it
    // here so `npm test` only runs the vitest unit suites.
    exclude: ["**/node_modules/**", "**/dist/**", "tests/e2e/**"],
  },
});
