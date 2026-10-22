// Downloads the engine's pinned embedding model into a folder, for a release to ship it:
//
//   node scripts/fetch-model.mjs <dir>       (after npm run build)
//
// The folder is laid out as transformers.js caches a pinned revision
// (<dir>/<model>/<revision>/<file>), which the engine reads with MEKO_MODELS_DIR and
// MEKO_MODELS_OFFLINE=1. transformers.js 4.3's pipeline() checks whether a model has a tokenizer at
// the "main" revision, whatever revision it then loads, so offline it would find none: the small
// JSON files are copied to <dir>/<model>/ too. The weights aren't.
import fs from "node:fs";
import path from "node:path";

import { DEFAULT_MODEL, WorkerEmbedder } from "../dist/embedder.js";

const dir = process.argv[2];
if (!dir) {
  console.error("usage: node scripts/fetch-model.mjs <dir>");
  process.exit(2);
}
fs.mkdirSync(dir, { recursive: true });
const e = new WorkerEmbedder(path.resolve(dir), DEFAULT_MODEL);
try {
  await e.ready();
  const v = await e.embed("a check that the model runs");
  if (v.length !== DEFAULT_MODEL.dims) throw new Error(`got ${v.length} dimensions, want ${DEFAULT_MODEL.dims}`);
} finally {
  await e.close();
}
const modelDir = path.join(dir, DEFAULT_MODEL.id);
const revDir = path.join(modelDir, DEFAULT_MODEL.revision);
for (const f of ["config.json", "tokenizer.json", "tokenizer_config.json"]) {
  fs.copyFileSync(path.join(revDir, f), path.join(modelDir, f));
}
console.log(`${DEFAULT_MODEL.id}@${DEFAULT_MODEL.revision} (${DEFAULT_MODEL.dtype}) is in ${dir}`);
