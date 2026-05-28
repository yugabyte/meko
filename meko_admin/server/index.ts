import express, { type Request, Response, NextFunction } from "express";
import { createServer } from "http";
import metricsRouter from "./routes/metrics";
import authRouter from "./routes/auth";
import { requireAdminAuth } from "./middleware/requireAdminAuth";
import { serveStatic } from "./static";

const app = express();
const httpServer = createServer(app);

app.use(express.json());
app.use(express.urlencoded({ extended: false }));

function log(message: string, source = "express") {
  const time = new Date().toLocaleTimeString("en-US", {
    hour: "numeric",
    minute: "2-digit",
    second: "2-digit",
    hour12: true,
  });
  console.log(`${time} [${source}] ${message}`);
}

app.use((req, res, next) => {
  const start = Date.now();
  res.on("finish", () => {
    if (req.path.startsWith("/api")) {
      log(`${req.method} ${req.path} ${res.statusCode} in ${Date.now() - start}ms`);
    }
  });
  next();
});

app.get("/api/admin/health", (_req, res) => res.json({ status: "ok" }));
app.use("/api/admin/auth", authRouter);
app.use("/api/admin/metrics", requireAdminAuth, metricsRouter);

(async () => {
  app.use((err: unknown, _req: Request, res: Response, _next: NextFunction) => {
    const status = (err as { status?: number })?.status ?? 500;
    const message = (err as { message?: string })?.message ?? "Internal Server Error";
    console.error("Internal Server Error:", err);
    if (!res.headersSent) res.status(status).json({ message });
  });

  if (process.env.NODE_ENV === "production") {
    serveStatic(app);
  } else {
    const { setupVite } = await import("./vite");
    await setupVite(httpServer, app);
  }

  const port = parseInt(process.env.PORT ?? "3011", 10);
  httpServer.listen(
    {
      port,
      host: "0.0.0.0",
      ...(process.platform === "linux" ? { reusePort: true as const } : {}),
    },
    () => {
      log(`Meko Admin serving on port ${port}`);
    },
  );
})();
