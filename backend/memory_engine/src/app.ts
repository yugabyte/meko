// Starts the memory engine: the embedder, mem0's providers, the API on its socket, then the hello that
// tells the Go server which embedding model it runs. It exits when the Go server goes away.
import fs from "node:fs";

import { HashEmbedder, WorkerEmbedder, type LocalEmbedder } from "./embedder.js";
import { MemoryEngine } from "./memoryengine.js";
import { register } from "./providers.js";
import { startServer, type Health } from "./server.js";
import { StoreClient, StoreError } from "./storeclient.js";

/** How often the memory engine checks that the Go server is still there. */
const PARENT_CHECK_MS = 1000;
/** The first and the longest wait between tries at loading the model and saying hello. */
const RETRY_MIN_MS = 1000;
const RETRY_MAX_MS = 60_000;

/** One JSON line per event on stderr, which the supervisor sends to logs/memory_engine.log. Ids and counts only. */
export function log(msg: string, fields: Record<string, unknown> = {}): void {
  process.stderr.write(JSON.stringify({ time: new Date().toISOString(), msg, ...fields }) + "\n");
}

function need(env: NodeJS.ProcessEnv, name: string): string {
  const v = env[name];
  if (!v) {
    log("missing environment variable; meko serve sets it", { name });
    process.exit(2);
  }
  return v;
}

export async function run(env: NodeJS.ProcessEnv): Promise<void> {
  const memoryEngineSocket = need(env, "MEKO_MEMORY_ENGINE_SOCKET");
  const storeSocket = need(env, "MEKO_STORE_SOCKET");
  const token = need(env, "MEKO_INTERNAL_TOKEN");
  const store = new StoreClient(storeSocket, token);
  const embedder: LocalEmbedder =
    env.MEKO_MEMORY_ENGINE_TEST_EMBEDDER === "hash" ? new HashEmbedder() : new WorkerEmbedder(need(env, "MEKO_MODELS_DIR"));
  register(store, embedder);
  const memoryEngine = new MemoryEngine(embedder);

  const health: Health = { state: "loading", embedder: embedder.info };
  const server = await startServer(memoryEngineSocket, token, memoryEngine, () => health, log);
  const ours = fs.statSync(memoryEngineSocket).ino;
  log("memory engine listening", { embedder: embedder.info });

  const stop = async (why: string) => {
    log("memory engine stopping", { why });
    server.close();
    // Only our own socket: a stale memory engine stopping late mustn't unlink its successor's.
    if (fs.statSync(memoryEngineSocket, { throwIfNoEntry: false })?.ino === ours) fs.rmSync(memoryEngineSocket, { force: true });
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

  await startUp(embedder, () => store.request("POST", "/v1/memory_engine/hello", embedder.info), health);
}

/**
 * Loads the model and says hello, retrying what can pass on its own: the store not answering yet,
 * a busy writer, a model download that failed offline. A different model already in the store
 * won't, so that one is an error until the user re-embeds.
 */
export async function startUp(
  embedder: Pick<LocalEmbedder, "ready" | "info">,
  hello: () => Promise<unknown>,
  health: Health,
  firstWait = RETRY_MIN_MS,
): Promise<void> {
  for (let wait = firstWait; ; wait = Math.min(wait * 2, RETRY_MAX_MS)) {
    try {
      await embedder.ready();
      await hello();
      health.state = "ready";
      delete health.error;
      log("memory engine ready", { embedder: embedder.info });
      return;
    } catch (e) {
      health.error = e instanceof Error ? e.message : String(e);
      const code = e instanceof StoreError ? e.code : (e as Error).name;
      if (e instanceof StoreError && e.code === "MODEL_CHANGED") {
        health.state = "error";
        log("memory engine can't start", { error: code });
        return;
      }
      log("memory engine not ready; retrying", { error: code, retry_ms: wait });
      await new Promise((r) => setTimeout(r, wait));
    }
  }
}
