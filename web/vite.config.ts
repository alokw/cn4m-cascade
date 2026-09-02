import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// The Go server's default listen address (internal/config/config.go).
const API = 'http://localhost:8384'

export default defineConfig({
  plugins: [react()],
  build: {
    // Consumed by web/embed.go and served by internal/api/spa.go, which treats
    // everything under assets/ as immutable. Keep the name in sync with
    // web.AssetDir if this ever changes.
    outDir: 'dist',
    assetsDir: 'assets',
    // Deliberately false. emptyOutDir wipes the whole directory, which deletes
    // the committed dist/.gitkeep that web/embed.go's //go:embed directive
    // needs in order to resolve on a tree that has never run a frontend build.
    // The npm build script clears dist/assets instead — that is the only place
    // stale hashed bundles can accumulate, and index.html is rewritten anyway.
    emptyOutDir: false,
  },
  server: {
    proxy: {
      '/api': {
        target: API,
        ws: true,
        changeOrigin: true,
        // The WS endpoint pins OriginPatterns to r.Host (internal/api/hub.go).
        // Through this proxy the browser sends Origin: http://localhost:5173
        // while r.Host is the proxy target, so coder/websocket rejects the
        // upgrade and the dev loop silently loses all live progress. Rewriting
        // Origin here keeps that fix in dev tooling rather than loosening the
        // check in production code.
        configure: (proxy) => {
          proxy.on('proxyReq', (proxyReq) => proxyReq.setHeader('origin', API))
        },
      },
    },
  },
})
