import preact from "@preact/preset-vite";
import { defineConfig } from "vite";

// The bundle every lha serve embeds: deterministic file names, no source maps, one CSS file.
export default defineConfig({
  plugins: [preact()],
  base: "/",
  build: {
    outDir: "dist",
    emptyOutDir: true,
    sourcemap: false,
    assetsDir: "assets",
    cssCodeSplit: false,
    modulePreload: { polyfill: false },
  },
  server: {
    // `npm run dev` against a running `lha serve` (LHA_SERVE_URL, default port 8765).
    proxy: { "/api": process.env.LHA_SERVE_URL ?? "http://127.0.0.1:8765" },
  },
});
