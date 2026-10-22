// Downloads the memory engine's pinned models (the embedder and the reranker) into a folder, for a
// release to ship them:
//
//   node scripts/fetch-model.mjs <dir>       (after npm run build)
//
// The folder is laid out as transformers.js caches a pinned revision
// (<dir>/<model>/<revision>/<file>), which the memory engine reads with MEKO_MODELS_DIR and
// MEKO_MODELS_OFFLINE=1. Each model is loaded and run once here, then loaded again with remote
// models off, so a folder that wouldn't work offline fails the build. transformers.js 4.3 looks for
// a model's tokenizer files at the "main" revision, whatever revision it then loads (pipeline() for
// the embedder, AutoTokenizer for the reranker), so offline it would find none: each model's small
// JSON files are copied to <dir>/<model>/ too. The weights aren't.
import fs from "node:fs";
import path from "node:path";

import { DEFAULT_MODEL, WorkerEmbedder } from "../dist/embedder.js";
import { DEFAULT_RERANKER, WorkerReranker } from "../dist/reranker.js";

const dir = process.argv[2];
if (!dir) {
  console.error("usage: node scripts/fetch-model.mjs <dir>");
  process.exit(2);
}
fs.mkdirSync(dir, { recursive: true });
const root = path.resolve(dir);

async function checkEmbedder(offline) {
  const e = new WorkerEmbedder(root, DEFAULT_MODEL, offline);
  try {
    await e.ready();
    const v = await e.embed("a check that the model runs");
    if (v.length !== DEFAULT_MODEL.dims) throw new Error(`got ${v.length} dimensions, want ${DEFAULT_MODEL.dims}`);
  } finally {
    await e.close();
  }
}

async function checkReranker(offline) {
  const r = new WorkerReranker(root, DEFAULT_RERANKER, offline);
  try {
    await r.ready();
    const [on, off] = await r.rerank("when do herons nest?", ["Herons nest in March.", "The build takes a minute."]);
    if (!(on > off)) throw new Error(`the reranker scored the matching text ${on}, below the other's ${off}`);
  } finally {
    await r.close();
  }
}

await checkEmbedder(false);
await checkReranker(false);
for (const m of [DEFAULT_MODEL, DEFAULT_RERANKER]) {
  const modelDir = path.join(root, m.id);
  for (const f of ["config.json", "tokenizer.json", "tokenizer_config.json"]) {
    fs.copyFileSync(path.join(modelDir, m.revision, f), path.join(modelDir, f));
  }
}
await checkEmbedder(true);
await checkReranker(true);
for (const m of [DEFAULT_MODEL, DEFAULT_RERANKER]) {
  console.log(`${m.id}@${m.revision} (${m.dtype}) is in ${dir}`);
}
