// The reranker worker thread: loads the cross-encoder once (downloading it into the models folder
// the first time, unless it's a release's bundled copy), then scores (query, text) pairs one batch at a time, replying with the model's
// raw logits.
import { parentPort, workerData } from "node:worker_threads";

import {
  AutoModelForSequenceClassification,
  AutoTokenizer,
  env,
  type PreTrainedModel,
  type PreTrainedTokenizer,
} from "@huggingface/transformers";

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

let tokenizer: PreTrainedTokenizer;
let classifier: PreTrainedModel;
try {
  tokenizer = await AutoTokenizer.from_pretrained(model, { revision });
  classifier = await AutoModelForSequenceClassification.from_pretrained(model, { revision, dtype: dtype as "q8" });
} catch (e) {
  port.postMessage({
    type: "load-error",
    message: `can't load the reranking model ${model} into ${cacheDir}: ${(e as Error).message}`,
  });
  process.exit(1);
}
port.postMessage({ type: "ready" });

let queue = Promise.resolve();
port.on("message", (m: { id: number; query: string; texts: string[] }) => {
  queue = queue.then(async () => {
    try {
      // Pairs longer than the model's 512 tokens are cut, the text's end first.
      const inputs = tokenizer(new Array<string>(m.texts.length).fill(m.query), {
        text_pair: m.texts,
        padding: true,
        truncation: true,
      });
      const { logits } = await classifier(inputs);
      const scores = (logits.tolist() as number[][]).map((row) => row[0]);
      port.postMessage({ type: "scores", id: m.id, scores });
    } catch (e) {
      port.postMessage({ type: "error", id: m.id, message: (e as Error).message });
    }
  });
});
