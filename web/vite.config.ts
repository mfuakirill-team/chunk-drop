import { defineConfig } from "vite";
import preact from "@preact/preset-vite";

// Vite config: Preact + TS app, built to ../web/dist where the Go
// `embed` picks it up (internal/webui).
export default defineConfig({
  plugins: [preact()],
  build: {
    outDir: "dist",
    emptyOutDir: true,
    // keep asset URLs relative so the app works from any host:port the
    // engine binds (QR links use the LAN IP, not localhost)
    assetsDir: "assets",
    rollupOptions: {
      output: {
        entryFileNames: "assets/[name].[hash].js",
        chunkFileNames: "assets/[name].[hash].js",
        assetFileNames: "assets/[name].[hash][extname]",
      },
    },
  },
  server: {
    proxy: {
      // dev server → engine API (run `engine` locally, it allows any
      // interface when no bind_ip is configured)
      "/api": "http://127.0.0.1:8090",
      "/r": "http://127.0.0.1:8090",
    },
  },
});