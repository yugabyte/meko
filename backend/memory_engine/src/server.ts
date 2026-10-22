// The memory engine API on run/mem_eng.sock: HTTP/JSON with the per-start internal token, in the same
// {data, error} envelope as the Go server. Only the Go server calls it.
import { timingSafeEqual } from "node:crypto";
import fs from "node:fs";
import http from "node:http";

import { HttpError, type MemoryEngine } from "./memoryengine.js";
import type { EmbedderInfo } from "./embedder.js";
import { StoreError } from "./storeclient.js";

/** Largest request body: an add or embed call with long texts. */
const MAX_BODY = 8 << 20;

export interface Health {
  state: "loading" | "ready" | "error";
  error?: string;
  embedder: EmbedderInfo;
}

type Handler = (memoryEngine: MemoryEngine, body: any) => Promise<unknown>;

const routes: Record<string, Handler> = {
  "/v1/add": (e, b) => e.add(b),
  "/v1/search": (e, b) => e.search(b),
  "/v1/update": (e, b) => e.update(b),
  "/v1/delete": (e, b) => e.delete(b),
  "/v1/embed": (e, b) => e.embed(b),
};

export function startServer(
  socket: string,
  token: string,
  memoryEngine: MemoryEngine,
  health: () => Health,
  log: (msg: string, fields?: Record<string, unknown>) => void,
): Promise<http.Server> {
  const want = Buffer.from(`Bearer ${token}`);
  const authorized = (h: string | undefined) => {
    const got = Buffer.from(h ?? "");
    return got.length === want.length && timingSafeEqual(got, want);
  };
  const reply = (res: http.ServerResponse, status: number, body: unknown) => {
    const s = JSON.stringify(body);
    res.writeHead(status, { "content-type": "application/json", "content-length": Buffer.byteLength(s) });
    res.end(s);
  };
  const fail = (res: http.ServerResponse, status: number, code: string, message: string) =>
    reply(res, status, { data: null, error: { code, message } });

  const server = http.createServer(async (req, res) => {
    const started = Date.now();
    const path = (req.url ?? "").split("?")[0];
    try {
      if (!authorized(req.headers.authorization)) {
        return fail(res, 401, "UNAUTHORIZED", "missing or wrong internal token");
      }
      if (req.method === "GET" && path === "/healthz") {
        return reply(res, 200, { data: health() });
      }
      const handler = routes[path];
      if (!handler || req.method !== "POST") return fail(res, 404, "NOT_FOUND", "no such route");
      const h = health();
      if (h.state !== "ready") {
        return fail(res, 503, "MEMORY_ENGINE_NOT_READY", h.error ?? "the memory engine is still starting");
      }
      const body = await readJSON(req);
      reply(res, 200, { data: await handler(memoryEngine, body) });
    } catch (e) {
      if (e instanceof HttpError) return fail(res, e.status, e.code, e.message);
      // The store's refusals (no such datapack, a bad filter) name no memory text: pass them on.
      if (e instanceof StoreError && e.status >= 400 && e.status < 500) return fail(res, e.status, e.code, e.message);
      // Never echo or log an internal error's message: it can carry memory text. The store's own
      // codes are safe to log.
      log("request failed", { route: path, error: e instanceof StoreError ? e.code : (e as Error).name });
      fail(res, 500, "INTERNAL_ERROR", "the memory engine couldn't complete the request");
    } finally {
      if (path !== "/healthz") log("request", { route: path, status: res.statusCode, ms: Date.now() - started });
    }
  });

  return new Promise((resolve, reject) => {
    fs.rmSync(socket, { force: true }); // a killed memory engine's socket
    server.once("error", reject);
    server.listen(socket, () => {
      fs.chmodSync(socket, 0o600);
      resolve(server);
    });
  });
}

/** The request's JSON body, which must be an object. */
function readJSON(req: http.IncomingMessage): Promise<Record<string, unknown>> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    let size = 0;
    req.on("data", (c: Buffer) => {
      if (size > MAX_BODY) return; // refused: the rest is read and dropped, so the 413 gets through
      size += c.length;
      if (size > MAX_BODY) {
        chunks.length = 0;
        return reject(new HttpError(413, "PAYLOAD_TOO_LARGE", "request body too large"));
      }
      chunks.push(c);
    });
    req.on("end", () => {
      let body: unknown;
      try {
        body = JSON.parse(Buffer.concat(chunks).toString("utf8") || "{}");
      } catch {
        return reject(new HttpError(400, "BAD_REQUEST", "the body isn't JSON"));
      }
      if (typeof body !== "object" || body === null || Array.isArray(body)) {
        return reject(new HttpError(400, "BAD_REQUEST", "the body must be a JSON object"));
      }
      resolve(body as Record<string, unknown>);
    });
    req.on("error", reject);
  });
}
