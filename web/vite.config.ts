import path from "node:path";
import tailwindcss from "@tailwindcss/vite";
import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";

const api = "http://127.0.0.1:8585";

// In development Vite serves the app and proxies Connect calls, MCP and the
// OIDC flow to the Go server. In production the Go binary serves the built
// app itself.
export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: { alias: { "@": path.resolve(__dirname, "src") } },
  server: {
    port: 5173,
    proxy: {
      "^/mandate\\.": { target: api, changeOrigin: true },
      "/auth/": { target: api, changeOrigin: true },
      "/mcp": { target: api, changeOrigin: true },
      "/healthz": { target: api, changeOrigin: true },
    },
  },
  build: { outDir: "dist", emptyOutDir: true, sourcemap: false },
});
