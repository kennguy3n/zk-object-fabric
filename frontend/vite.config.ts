import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// Vite config for the tenant console. The console talks to the
// gateway's /api/v1/ management API (separate from the
// S3-compatible routes on /bucket/key). In dev the gateway is
// proxied through Vite so the SPA can make same-origin requests and
// avoid CORS preflight noise.
export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      "/api/v1": {
        target: "http://localhost:8080",
        changeOrigin: true,
      },
    },
  },
});
