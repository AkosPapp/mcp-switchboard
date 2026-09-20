// defineConfig comes from vitest rather than vite so the `test` block below is
// part of the same typed config instead of a second file to keep in step.
import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";

// The console is served from the hub binary, which embeds dist/ (spec.md U1).
//
// Assets are referenced from the root rather than relatively: every unmatched
// path serves index.html so client-side routing works, and a relative asset URL
// resolves against whatever route the browser is showing - "/calls/" would ask
// for "/calls/assets/...". Absolute URLs are correct at any depth. A hub behind
// a path-prefixing proxy needs this set to that prefix at build time.
export default defineConfig({
  plugins: [react()],
  base: "/",
  build: {
    outDir: "dist",
    emptyOutDir: true,
    // Source maps would double what the binary carries for no benefit: the
    // console ships minified and its errors are reported through the UI.
    sourcemap: false,
  },
  server: {
    // `npm run dev` proxies to a hub on its default private port, so the
    // console can be developed against a real one.
    proxy: {
      "/api": "http://127.0.0.1:8099",
      "/mcp": "http://127.0.0.1:8099",
    },
  },
  test: {
    environment: "jsdom",
    globals: true,
    setupFiles: ["./src/test/setup.ts"],
    css: false,
    // e2e/ belongs to Playwright, which needs a real browser and a running hub;
    // vitest would collect those specs and fail on the first import.
    include: ["src/**/*.{test,spec}.{ts,tsx}"],
  },
});
