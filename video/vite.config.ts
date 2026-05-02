import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';
import { resolve } from 'node:path';

// Vite config for the embeddable Player demo.
// `npm run dev`        — local preview at http://localhost:5173/
// `npm run build:player` — static bundle in dist/ ready to host on GitHub Pages
export default defineConfig({
  plugins: [react()],
  publicDir: resolve(__dirname, 'public'),
  // Relative base so the bundle works at any subpath — required for the
  // GitHub Pages embed where the player lives at /foxy-switcher/intro/
  // and the parent site loads it via iframe src="intro/".
  base: './',
  build: {
    outDir: resolve(__dirname, 'dist'),
    emptyOutDir: true,
    sourcemap: true,
  },
  // Avoid deep-bundling Remotion's CLI on this surface.
  optimizeDeps: {
    exclude: ['@remotion/cli'],
  },
});
