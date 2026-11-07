// A stand-in for the Go server's store API, in memory, for the context engine's own tests: enough of each
// route's behavior (equality filters, cosine search, term-count keyword scores) to drive mem0. The
// real store API is tested in Go (internal/storeapi).
import fs from "node:fs";
import http from "node:http";
import os from "node:os";
import path from "node:path";

interface Row {
  vector: number[];
  payload: Record<string, any>;
}

export interface FakeStore {
  socket: string;
  token: string;
  rows: Map<string, Map<string, Row>>; // collection -> id -> row
  history: any[];
  requests: Array<{ method: string; path: string; body: any }>;
  close(): Promise<void>;
}

const matches = (p: Record<string, any>, f: Record<string, any> = {}) =>
  Object.entries(f).every(([k, v]) => v === "*" || p[k] === v);

const cosine = (a: number[], b: number[]) => a.reduce((s, x, i) => s + x * b[i], 0);

export async function startFakeStore(): Promise<FakeStore> {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "mk-"));
  const fake: FakeStore = {
    socket: path.join(dir, "store.sock"),
    token: "test-token",
    rows: new Map(),
    history: [],
    requests: [],
    close: async () => {
      await new Promise((r) => server.close(r));
      fs.rmSync(dir, { recursive: true, force: true });
    },
  };
  const col = (name: string) => {
    if (!fake.rows.has(name)) fake.rows.set(name, new Map());
    return fake.rows.get(name)!;
  };
  const server = http.createServer((req, res) => {
    const chunks: Buffer[] = [];
    req.on("data", (c: Buffer) => chunks.push(c));
    req.on("end", () => {
      const body = chunks.length ? JSON.parse(Buffer.concat(chunks).toString()) : undefined;
      const url = req.url ?? "";
      fake.requests.push({ method: req.method ?? "", path: url, body });
      const send = (data: unknown, status = 200, error?: { code: string; message: string }) => {
        res.writeHead(status, { "content-type": "application/json" });
        res.end(JSON.stringify(error ? { data: null, error } : { data }));
      };
      if (req.headers.authorization !== `Bearer ${fake.token}`) {
        return send(null, 401, { code: "UNAUTHORIZED", message: "token" });
      }
      let m: RegExpMatchArray | null;
      if ((m = url.match(/^\/v1\/collections\/([^/]+)\/(insert|search|keyword-search|list)$/))) {
        const c = col(decodeURIComponent(m[1]));
        if (m[2] === "insert") {
          body.ids.forEach((id: string, i: number) => c.set(id, { vector: body.vectors[i], payload: body.payloads[i] }));
          return send({ inserted: body.ids.length });
        }
        const hits = [...c.entries()].filter(([, r]) => matches(r.payload, body.filters));
        if (m[2] === "list") {
          const rows = hits.map(([id, r]) => ({ id, payload: r.payload }));
          return send({ results: rows.slice(0, body.top_k || 100), total: rows.length });
        }
        if (m[2] === "search") {
          const scored = hits.map(([id, r]) => ({ id, payload: r.payload, score: cosine(body.vector, r.vector) }));
          scored.sort((a, b) => b.score - a.score);
          return send({ results: scored.slice(0, body.top_k) });
        }
        const terms = String(body.query).split(/\s+/).filter(Boolean);
        const scored = hits
          .map(([id, r]) => {
            const words = String(r.payload.textLemmatized ?? r.payload.data).toLowerCase().split(/\s+/);
            return { id, payload: r.payload, score: terms.filter((t) => words.includes(t)).length * 10 };
          })
          .filter((x) => x.score > 0);
        return send({ results: scored });
      }
      if ((m = url.match(/^\/v1\/collections\/([^/]+)\/vectors\/([^/]+)$/))) {
        const c = col(decodeURIComponent(m[1]));
        const id = decodeURIComponent(m[2]);
        if (req.method === "GET") {
          const r = c.get(id);
          return send(r ? { id, payload: r.payload } : null);
        }
        if (req.method === "PUT") {
          if (!c.has(id)) return send(null, 404, { code: "NOT_FOUND", message: "no such row" });
          c.set(id, { vector: body.vector, payload: body.payload });
          return send({ ok: true });
        }
        c.delete(id);
        return send({ ok: true });
      }
      if (url.startsWith("/v1/history/") && req.method === "POST") {
        fake.history.push(...body.entries);
        return send({ added: body.entries.length });
      }
      if (url === "/v1/context_engine/hello") return send({ ok: true });
      send(null, 404, { code: "NOT_FOUND", message: "no route" });
    });
  });
  await new Promise<void>((r) => server.listen(fake.socket, r));
  return fake;
}
