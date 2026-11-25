// The embedding worker thread: loads the model once (downloading it into the models folder the
// first time, unless it's a release's bundled copy), then embeds batches of texts one at a time,
// mean-pooled and normalized.
import { parentPort, workerData } from "node:worker_threads";

import { env, pipeline, type FeatureExtractionPipeline } from "@huggingface/transformers";

const { cacheDir, model, revision, dtype, offline } = workerData as {
  cacheDir: string;
  model: string;
  revision: string;
  dtype: string;
  offline: boolean;
};
const port = parentPort!;

env.cacheDir = cacheDir;
// A release ships the model: never fetch it, so a missing file says so instead of reaching out.
env.allowRemoteModels = !offline;

let extractor: FeatureExtractionPipeline;
try {
  extractor = await pipeline("feature-extraction", model, { revision, dtype: dtype as "q8" });
} catch (e) {
  port.postMessage({
    type: "load-error",
    message: `can't load the embedding model ${model} into ${cacheDir}: ${(e as Error).message}`,
  });
  process.exit(1);
}
port.postMessage({ type: "ready" });

let queue = Promise.resolve();
port.on("message", (m: { id: number; texts: string[] }) => {
  queue = queue.then(async () => {
    try {
      const out = await extractor(m.texts, { pooling: "mean", normalize: true });
      port.postMessage({ type: "vectors", id: m.id, vectors: out.tolist() });
    } catch (e) {
      port.postMessage({ type: "error", id: m.id, message: (e as Error).message });
    }
  });
});
