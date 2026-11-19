import { defineConfig } from "vite";

// The Go server owns /api and /ws on :8787. In dev the client is served by Vite
// on :5184, so proxy those through — otherwise the client's default server
// address (its own origin) points at Vite, which has no API and 404s.
const API = process.env.GLIPH_API ?? "http://localhost:8787";

// A production build pins the server it talks to (`GLIPH_SERVER=https://gliph.sh
// npm run build`). The desktop and mobile shells load the same bundle from a
// tauri:// origin, which is not a server address — without the
// pin they open on an empty field. Blank here means "use your own origin",
// which is what dev and self-hosters want.
const SERVER = process.env.GLIPH_SERVER ?? "";
// `bin/version --long` at build time; "dev" under `vite dev`. Shown by /status.
const VERSION = process.env.GLIPH_VERSION ?? "dev";

// In production the Go server maps /admin and /admin/* onto admin.html (see
// http.go). Vite's dev server only knows about files, so it needs the same
// rewrite or the console's own deep links 404 in dev and work in prod — the
// worst way round to find out.
const adminRoute = {
  name: "gliph-admin-route",
  configureServer(dev: { middlewares: { use: (fn: (req: { url?: string }, res: unknown, next: () => void) => void) => void } }) {
    dev.middlewares.use((req, _res, next) => {
      if (req.url && /^\/admin(?:[/?#]|$)/.test(req.url)) req.url = "/admin.html";
      next();
    });
  },
};

export default defineConfig({
  plugins: [adminRoute],
  define: { __GLIPH_SERVER__: JSON.stringify(SERVER), __GLIPH_VERSION__: JSON.stringify(VERSION) },
  server: {
    port: 5184,
    strictPort: true,
    proxy: {
      "/api": { target: API, changeOrigin: true },
      "/ws": { target: API, changeOrigin: true, ws: true },
      "/healthz": { target: API, changeOrigin: true },
    },
  },
  // five pages: gliph.sh's front page (/landing.html, server/landing.go), the client, the admin console (/admin.html), the device
  // approval page (/link.html) the native apps open in a real browser, and
  // /notify.html — the desktop app's always-on-top notification window, which is
  // its own OS window and therefore its own document.
  build: {
    target: "es2022",
    rollupOptions: {
      input: {
        main: "index.html", admin: "admin.html",
        link: "link.html", notify: "notify.html",
        landing: "landing.html",
      },
    },
  },
});
