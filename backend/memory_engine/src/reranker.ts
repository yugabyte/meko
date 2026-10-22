// Rerankers for knowledge-base search: Meko Cloud's cross-encoder on transformers.js, run in a
// worker thread of its own so reranking never waits behind the embedder, and a word-overlap
// reranker for tests that can't download a model.
import { Worker } from "node:worker_threads";

/** What the engine reports about its reranker. */
export interface RerankerInfo {
  provider: string;
  model: string;
}

export interface LocalReranker {
  readonly info: RerankerInfo;
  /** loading until the model is in; error (with why) when a load failed or the worker died. */
  readonly state: "loading" | "ready" | "error";
  readonly error?: string;
  /** Resolves once the model is loaded; rejects if it can't be. A failed worker is replaced. */
  ready(): Promise<void>;
  /**
   * Starts replacing a failed worker, at most once every REVIVE_MS, without waiting for it: called
   * when a rerank finds the reranker in error, so a worker that died after loading comes back.
   */
  revive(): void;
  /** The model's raw score (a logit: higher is more relevant) for each text against the query. */
  rerank(query: string, texts: string[]): Promise<number[]>;
  close(): Promise<void>;
}

/**
 * Meko Cloud's reranker, cross-encoder/ms-marco-MiniLM-L-6-v2, as its ONNX build, 8-bit quantized
 * (about 23 MB), at a pinned commit of its Hugging Face repo so every install downloads the same
 * weights.
 */
export const DEFAULT_RERANKER = {
  id: "Xenova/ms-marco-MiniLM-L-6-v2",
  revision: "a09144355adeed5f58c8ed011d209bf8ee5a1fec",
  dtype: "q8",
};

/** Pairs per call to the model. */
const BATCH = 16;

/** The least time between two tries at replacing a failed worker, so a model that can't load isn't reloaded by every search. */
export const REVIVE_MS = 30_000;

type Pending = { resolve: (v: number[]) => void; reject: (e: Error) => void };

/**
 * The cross-encoder, in a worker thread. A worker that failed (to load, or later) is replaced on the
 * next ready(), which revive() calls.
 */
export class WorkerReranker implements LocalReranker {
  readonly info: RerankerInfo;
  private worker!: Worker;
  private started = 0;
  private loaded!: Promise<void>;
  private isLoaded = false;
  private readonly pending = new Map<number, Pending>();
  private nextId = 1;
  private failure?: Error;

  /**
   * cacheDir holds the model files; offline means they're a release's bundled copy, so a missing
   * file is an error rather than a download.
   */
  constructor(
    private readonly cacheDir: string,
    private readonly model = DEFAULT_RERANKER,
    private readonly offline = false,
  ) {
    this.info = { provider: "transformers.js", model: `${model.id}@${model.revision}:${model.dtype}` };
    this.start();
  }

  get state(): "loading" | "ready" | "error" {
    if (this.failure) return "error";
    return this.isLoaded ? "ready" : "loading";
  }

  get error(): string | undefined {
    return this.failure?.message;
  }

  private start(): void {
    this.failure = undefined;
    this.isLoaded = false;
    this.started = Date.now();
    const { cacheDir, model, offline } = this;
    const worker = new Worker(new URL("./rerankworker.js", import.meta.url), {
      workerData: { cacheDir, model: model.id, revision: model.revision, dtype: model.dtype, offline },
    });
    this.worker = worker;
    this.loaded = new Promise((resolve, reject) => {
      worker.on("message", (m: { type: string; id?: number; scores?: number[]; message?: string }) => {
        if (m.type === "ready") {
          this.isLoaded = true;
          return resolve();
        }
        if (m.type === "load-error") return this.fail(worker, new Error(m.message), reject);
        const p = this.pending.get(m.id!);
        if (!p) return;
        this.pending.delete(m.id!);
        if (m.type === "scores") p.resolve(m.scores!);
        else p.reject(new Error(m.message));
      });
      worker.on("error", (e) => this.fail(worker, e, reject));
      worker.on("exit", (code) => this.fail(worker, new Error(`the reranking worker exited (${code})`), reject));
    });
    this.loaded.catch(() => undefined); // read through ready()
  }

  private fail(worker: Worker, e: Error, reject: (e: Error) => void): void {
    reject(e);
    if (worker !== this.worker) return; // a replaced worker's exit
    this.isLoaded = false;
    this.failure ??= e;
    for (const p of this.pending.values()) p.reject(this.failure);
    this.pending.clear();
  }

  ready(): Promise<void> {
    if (this.failure) {
      void this.worker.terminate();
      this.start();
    }
    return this.loaded;
  }

  revive(): void {
    if (this.failure && Date.now() - this.started >= REVIVE_MS) this.ready().catch(() => undefined);
  }

  async rerank(query: string, texts: string[]): Promise<number[]> {
    const out: number[] = [];
    for (let i = 0; i < texts.length; i += BATCH) {
      out.push(...(await this.call(query, texts.slice(i, i + BATCH))));
    }
    return out;
  }

  private call(query: string, texts: string[]): Promise<number[]> {
    if (this.failure) return Promise.reject(this.failure);
    const id = this.nextId++;
    return new Promise((resolve, reject) => {
      this.pending.set(id, { resolve, reject });
      this.worker.postMessage({ id, query, texts });
    });
  }

  async close(): Promise<void> {
    await this.worker.terminate();
  }
}

/**
 * For tests only (with the hashing embedder): the share of the query's words a text contains,
 * mapped to a logit from -4 (none) to 4 (all), so tests can tell a relevant text from others.
 */
export class OverlapReranker implements LocalReranker {
  readonly info: RerankerInfo = { provider: "test", model: "overlap" };
  readonly state = "ready" as const;

  async ready(): Promise<void> {}

  revive(): void {}

  async rerank(query: string, texts: string[]): Promise<number[]> {
    const q = new Set(words(query));
    return texts.map((t) => {
      if (q.size === 0) return -4;
      const have = new Set(words(t));
      let n = 0;
      for (const w of q) if (have.has(w)) n++;
      return -4 + (8 * n) / q.size;
    });
  }

  async close(): Promise<void> {}
}

function words(s: string): string[] {
  return s.toLowerCase().match(/[\p{L}\p{N}_]+/gu) ?? [];
}
