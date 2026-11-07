// Starts the context engine: the embedder, mem0's providers, the API on its socket, then the hello that
// tells the Go server which embedding model it runs. It exits when the Go server goes away.
import fs from "node:fs";

import { HashEmbedder, WorkerEmbedder, type LocalEmbedder } from "./embedder.js";
import { ContextEngine } from "./contextengine.js";
import { register } from "./providers.js";
import { OverlapReranker, WorkerReranker, type LocalReranker } from "./reranker.js";
import { startServer, type Health } from "./server.js";
import { StoreClient, StoreError } from "./storeclient.js";

/** How often the context engine checks that the Go server is still there. */
const PARENT_CHECK_MS = 1000;
/** The first and the longest wait between tries at loading the model and saying hello. */
const RETRY_MIN_MS = 1000;
const RETRY_MAX_MS = 60_000;

/** One JSON line per event on stderr, which the supervisor sends to logs/context-engine.log. Ids and counts only. */
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
  const contextEngineSocket = need(env, "MEKO_CONTEXT_ENGINE_SOCKET");
  const storeSocket = need(env, "MEKO_STORE_SOCKET");
  const token = need(env, "MEKO_INTERNAL_TOKEN");
  const store = new StoreClient(storeSocket, token);
  const testing = env.MEKO_CONTEXT_ENGINE_TEST_EMBEDDER === "hash";
  // A release's bundled models are read as they are: nothing is downloaded (MEKO_MODELS_OFFLINE).
  const offline = env.MEKO_MODELS_OFFLINE === "1";
  const embedder: LocalEmbedder = testing
    ? new HashEmbedder()
    : new WorkerEmbedder(need(env, "MEKO_MODELS_DIR"), undefined, offline);
  const reranker: LocalReranker = testing
    ? new OverlapReranker()
    : new WorkerReranker(need(env, "MEKO_MODELS_DIR"), undefined, offline);
  register(store, embedder);
  const contextEngine = new ContextEngine(embedder, undefined, reranker);

  const health: Health = { state: "loading", embedder: embedder.info };
  const report = (): Health => ({
    ...health,
    reranker: { ...reranker.info, state: reranker.state, ...(reranker.error && { error: reranker.error }) },
  });
  const server = await startServer(contextEngineSocket, token, contextEngine, report, log);
  const ours = fs.statSync(contextEngineSocket).ino;
  log("context engine listening", { embedder: embedder.info });

  const stop = async (why: string) => {
    log("context engine stopping", { why });
    server.close();
    // Only our own socket: a stale context engine stopping late mustn't unlink its successor's.
    if (fs.statSync(contextEngineSocket, { throwIfNoEntry: false })?.ino === ours) fs.rmSync(contextEngineSocket, { force: true });
    await Promise.all([embedder.close(), reranker.close()]);
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

  await startUp(embedder, () => store.request("POST", "/v1/context_engine/hello", embedder.info), health);
  void loadReranker(reranker);
}

/**
 * Loads the reranker after the embedder, retrying a failed load (a download that failed offline)
 * with the same backoff. The context engine is ready without it: knowledge-base search then keeps its
 * candidates' fused order.
 */
export async function loadReranker(reranker: Pick<LocalReranker, "ready">, firstWait = RETRY_MIN_MS): Promise<void> {
  for (let wait = firstWait; ; wait = Math.min(wait * 2, RETRY_MAX_MS)) {
    try {
      await reranker.ready();
      log("reranker ready");
      return;
    } catch (e) {
      log("reranker not ready; retrying", { error: (e as Error).name, retry_ms: wait });
      await new Promise((r) => setTimeout(r, wait));
    }
  }
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
      log("context engine ready", { embedder: embedder.info });
      return;
    } catch (e) {
      health.error = e instanceof Error ? e.message : String(e);
      const code = e instanceof StoreError ? e.code : (e as Error).name;
      if (e instanceof StoreError && e.code === "MODEL_CHANGED") {
        health.state = "error";
        log("context engine can't start", { error: code });
        return;
      }
      log("context engine not ready; retrying", { error: code, retry_ms: wait });
      await new Promise((r) => setTimeout(r, wait));
    }
  }
}
