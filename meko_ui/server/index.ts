import express, { type Request, Response, NextFunction } from "express";
import { createProxyMiddleware } from "http-proxy-middleware";
import { registerRoutes } from "./routes";
import { serveStatic } from "./static";
import { createServer } from "http";

// MEKO_API_UPSTREAM: Go api_server base URL; empty disables. Dev default http://127.0.0.1:25433.
function mekoApiUpstream(): string | undefined {
  if (process.env.MEKO_API_UPSTREAM !== undefined) {
    const trimmed = process.env.MEKO_API_UPSTREAM.trim();
    return trimmed || undefined;
  }
  if (process.env.NODE_ENV === "development") {
    return "http://127.0.0.1:25433";
  }
  return undefined;
}

const app = express();
const httpServer = createServer(app);

declare module "http" {
  interface IncomingMessage {
    rawBody: unknown;
  }
}

const apiUpstream = mekoApiUpstream();

// Admin console BFF gate. api_server's /api/v1/admin/* endpoints require the
// shared X-Meko-Admin-Secret header, which the browser must never hold. We
// inject it here, server-side, on admin requests — but ONLY when the Admin view
// is enabled (SHOW_ADMIN_VIEW) AND the secret (ADMIN_API_KEY, the same var
// meko_admin uses) is configured. When either is unset, admin requests forward
// without the header and api_server rejects them — the safe default that keeps
// admin data unreachable until an operator opts in.
//
// SECURITY — interim posture: there is NO per-user admin role yet. When this is
// enabled, the secret is injected for EVERY authenticated request to
// /api/v1/admin/*, and api_server authorizes those routes on the shared secret
// alone. So ANY signed-in user who reaches /api/v1/admin/* gets full admin
// access (all users' PII, tier changes, AMP credential read/replace/delete) —
// the client-side flag/localStorage only hides the UI, it does NOT gate the API.
// Therefore: enable SHOW_ADMIN_VIEW ONLY in trusted/dev/single-operator
// environments. Do NOT set it in a shared/multi-tenant/production deployment
// until per-user admin-role gating lands (which must gate BOTH the sidebar and
// this proxy). Tracked as the follow-up to this integration.
const adminSecret = (process.env.ADMIN_API_KEY ?? "").trim();
const adminViewEnabled = (process.env.SHOW_ADMIN_VIEW ?? "").trim().toLowerCase() === "true";
const adminProxyEnabled = adminViewEnabled && adminSecret !== "";

if (apiUpstream) {
  if (adminProxyEnabled) {
    log("admin proxy enabled: injecting X-Meko-Admin-Secret on /api/v1/admin/*");
  }
  // Do not mount at "/api/v1" — Express strips the mount from req.url and the
  // proxy would forward paths without the /api/v1 prefix.
  app.use(
    createProxyMiddleware({
      target: apiUpstream,
      changeOrigin: true,
      pathFilter: "/api/v1",
      on: {
        proxyReq: (proxyReq, req) => {
          if (adminProxyEnabled && (req.url ?? "").startsWith("/api/v1/admin")) {
            proxyReq.setHeader("X-Meko-Admin-Secret", adminSecret);
          }
        },
      },
    }),
  );
}

app.use(
  express.json({
    verify: (req, _res, buf) => {
      req.rawBody = buf;
    },
  }),
);

app.use(express.urlencoded({ extended: false }));

export function log(message: string, source = "express") {
  const formattedTime = new Date().toLocaleTimeString("en-US", {
    hour: "numeric",
    minute: "2-digit",
    second: "2-digit",
    hour12: true,
  });

  console.log(`${formattedTime} [${source}] ${message}`);
}

app.use((req, res, next) => {
  const start = Date.now();
  const path = req.path;
  let capturedJsonResponse: Record<string, any> | undefined = undefined;

  const originalResJson = res.json;
  res.json = function (bodyJson, ...args) {
    capturedJsonResponse = bodyJson;
    return originalResJson.apply(res, [bodyJson, ...args]);
  };

  res.on("finish", () => {
    const duration = Date.now() - start;
    if (path.startsWith("/api")) {
      let logLine = `${req.method} ${path} ${res.statusCode} in ${duration}ms`;
      if (capturedJsonResponse) {
        logLine += ` :: ${JSON.stringify(capturedJsonResponse)}`;
      }

      log(logLine);
    }
  });

  next();
});

(async () => {
  if (apiUpstream) {
    log(`proxy /api/v1 -> ${apiUpstream}`);
  }
  await registerRoutes(httpServer, app);

  // Runtime config for the browser. VITE_/MEKO_ vars are normally inlined into
  // the bundle at build time; this endpoint instead reads them from the pod's
  // env at request time and exposes an allow-listed set as
  // `window.__MEKO_RUNTIME__`, so they can be changed per-deployment without a
  // rebuild. index.html loads this before the app bundle; the client reads it
  // via readRuntimeEnv() with a build-time import.meta.env fallback.
  const RUNTIME_ENV_KEYS = [
    "MEKO_FREE_TIER_QUOTA_ENABLED",
    "MEKO_FREE_TIER_DATAPACKS",
    "MEKO_FREE_TIER_KB_BYTES",
    "MEKO_FREE_TIER_CONVERSATIONS",
    "MEKO_FREE_TIER_TOKEN_BURN",
    "VITE_RUDDERSTACK_WRITE_KEY",
    "VITE_RUDDERSTACK_DATA_PLANE_URL",
    // Gates the AMP (Postgres databases) UI inside a datapack. Runtime flag so
    // it can be flipped per environment without a rebuild. Default off.
    "SHOW_PG_AMP",
    // Gates the "Meko efficiency" token-burn tiles. Mirrors the BE/MCP flag of
    // the same name so the tiles only show where contextual prominence search
    // (which produces the token-savings data) is enabled. Default off.
    "MEKO_ENABLE_CONTEXTUAL_PROMINENCE_SEARCH",
    // Gates the in-app Admin console (Data Plane / Metrics / Executive / Users /
    // AMP) and its left-nav tab. Off by default. When on, the server also
    // injects the admin secret on /api/v1/admin/* (see adminProxyEnabled above),
    // so ADMIN_API_KEY must be set too for the console to load data.
    "SHOW_ADMIN_VIEW",
  ];
  app.get("/runtime-config.js", (_req, res) => {
    const cfg: Record<string, string> = {};
    for (const key of RUNTIME_ENV_KEYS) {
      const val = process.env[key];
      if (val !== undefined) cfg[key] = val;
    }
    res.type("application/javascript");
    res.setHeader("Cache-Control", "no-store");
    res.send(`window.__MEKO_RUNTIME__ = ${JSON.stringify(cfg)};`);
  });

  app.use((err: any, _req: Request, res: Response, next: NextFunction) => {
    const status = err.status || err.statusCode || 500;
    const message = err.message || "Internal Server Error";

    console.error("Internal Server Error:", err);

    if (res.headersSent) {
      return next(err);
    }

    return res.status(status).json({ message });
  });

  // importantly only setup vite in development and after
  // setting up all the other routes so the catch-all route
  // doesn't interfere with the other routes
  if (process.env.NODE_ENV === "production") {
    serveStatic(app);
  } else {
    const { setupVite } = await import("./vite");
    await setupVite(httpServer, app);
  }

  // ALWAYS serve the app on the port specified in the environment variable PORT
  // Other ports are firewalled. Default to 5000 if not specified.
  // this serves both the API and the client.
  // It is the only port that is not firewalled.
  const port = parseInt(process.env.PORT || "3010", 10);
  httpServer.listen(
    {
      port,
      host: "0.0.0.0",
      // reusePort enables SO_REUSEPORT on Linux (e.g. multiple workers on one port). macOS/Windows do not support it.
      // Guard with platform so dev/prod on those OSes call listen with only port + host and avoid ENOTSUP.
      ...(process.platform === "linux" ? { reusePort: true as const } : {}),
    },
    () => {
      log(`serving on port ${port}`);
    },
  );
})();
