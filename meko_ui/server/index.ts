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
if (apiUpstream) {
  // Do not mount at "/api/v1" — Express strips the mount from req.url and the
  // proxy would forward paths without the /api/v1 prefix.
  app.use(
    createProxyMiddleware({
      target: apiUpstream,
      changeOrigin: true,
      pathFilter: "/api/v1",
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
