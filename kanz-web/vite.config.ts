import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// The SPA is served BY THE BFF in every deployed shape (#371): one origin, so the
// httpOnly session cookie needs no CORS and no cookie-domain syncing.
//
// This proxy exists only for `npm run dev`, where Vite serves the app on its own
// port. It forwards /api and /auth to the BFF so the DEV shape has the same
// same-origin behaviour as production — otherwise the cookie silently stops
// working the moment you stop using the dev server, which is the worst possible
// time to discover it.
//
// BFF_INTERNAL_PORT is read from the environment so the local rig can move the
// BFF without editing this file; 8084 matches WEB_BFF_LISTEN's default.
const bff = `http://127.0.0.1:${process.env.BFF_INTERNAL_PORT ?? '8084'}`

export default defineConfig({
  plugins: [vue()],
  server: {
    // 5173 is Vite's default and collides with nothing in the local rig
    // (Postgres 5432, NATS 4222/4223, Kafka 9092, Redis 6379, BFF 8084).
    port: 5173,
    strictPort: true,
    proxy: {
      '/api': { target: bff, changeOrigin: false },
      '/auth': { target: bff, changeOrigin: false },
    },
  },
  build: {
    // Where the BFF's WEB_BFF_STATIC_DIR points.
    outDir: 'dist',
    emptyOutDir: true,
  },
})
