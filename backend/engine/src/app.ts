// Starts the engine: the embedder, mem0's providers, the API on its socket, then the hello that
// tells the Go server which embedding model it runs. It exits when the Go server goes away.
import fs from "node:fs";

import { HashEmbedder, WorkerEmbedder, type LocalEmbedder } from "./embedder.js";
import { Engine } from "./engine.js";
import { register } from "./providers.js";
import { startServer, type Health } from "./server.js";
import { StoreClient, StoreError } from "./storeclient.js";

/** How often the engine checks that the Go server is still there. */
const PARENT_CHECK_MS = 1000;

/** One JSON line per event on stderr, which the supervisor sends to logs/engine.log. Ids and counts only. */
export function log(msg: string, fields: Record<string, unknown> = {}): void {
  process.stderr.write(JSON.stringify({ time: new Date().toISOString(), msg, ...fields }) + "\n");
}

function need(env: NodeJS.ProcessEnv, name: string): string {
  const v = env[name];
  if (!v) {
    log("missing environment variable; meko-local serve sets it", { name });
    process.exit(2);
  }
  return v;
}

export async function run(env: NodeJS.ProcessEnv): Promise<void> {
  const engineSocket = need(env, "MEKO_ENGINE_SOCKET");
  const storeSocket = need(env, "MEKO_STORE_SOCKET");
  const token = need(env, "MEKO_INTERNAL_TOKEN");
  const store = new StoreClient(storeSocket, token);
  const embedder: LocalEmbedder =
    env.MEKO_ENGINE_TEST_EMBEDDER === "hash" ? new HashEmbedder() : new WorkerEmbedder(need(env, "MEKO_MODELS_DIR"));
  register(store, embedder);
  const engine = new Engine(embedder);

  const health: Health = { state: "loading", embedder: embedder.info };
  const server = await startServer(engineSocket, token, engine, () => health, log);
  log("engine listening", { embedder: embedder.info });

  const stop = async (why: string) => {
    log("engine stopping", { why });
    server.close();
    fs.rmSync(engineSocket, { force: true });
    await embedder.close();
    process.exit(0);
  };
  process.on("SIGTERM", () => void stop("SIGTERM"));
  process.on("SIGINT", () => void stop("SIGINT"));

  // A backstop to the supervisor's process-group kill: a parent that died without stopping us.
  const parent = process.ppid;
  let missing = 0;
  setInterval(() => {
    missing = fs.existsSync(storeSocket) ? 0 : missing + 1;
    if (process.ppid !== parent || missing >= 3) void stop("the Go server is gone");
  }, PARENT_CHECK_MS).unref();

  try {
    await embedder.ready();
    await store.request("POST", "/v1/engine/hello", embedder.info);
    health.state = "ready";
    log("engine ready", { embedder: embedder.info });
  } catch (e) {
    // Stay up and say why: the supervisor would only restart into the same failure.
    health.state = "error";
    health.error = e instanceof StoreError || e instanceof Error ? e.message : String(e);
    log("engine can't start", { error: e instanceof StoreError ? e.code : (e as Error).name });
  }
}
