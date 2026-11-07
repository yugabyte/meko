// Embedders: the local model on transformers.js, run in a worker thread so embedding never blocks
// the context engine's HTTP loop, and a deterministic hashing embedder for tests that can't download a
// model.
import { createHash } from "node:crypto";
import { Worker } from "node:worker_threads";

import type { Embedder } from "mem0ai/oss";

/** What the context engine reports to the Go server: the vector tables are made at this width. */
export interface EmbedderInfo {
  provider: string;
  model: string;
  dims: number;
}

export interface LocalEmbedder extends Embedder {
  readonly info: EmbedderInfo;
  /**
   * Embeds texts behind any search's: background batches (knowledge files, turns for conversation
   * search) wait while a query is queued, so a query waits for at most the one batch being embedded.
   */
  embedBackground(texts: string[]): Promise<number[][]>;
  /** Resolves once the model is loaded; rejects if it can't be. */
  ready(): Promise<void>;
  close(): Promise<void>;
}

/**
 * The default model: 384 dimensions, 8-bit quantized (about 23 MB), at a pinned commit of its
 * Hugging Face repo so every install downloads the same weights. The commit is part of the name the
 * store's model lock compares.
 */
export const DEFAULT_MODEL = {
  id: "Xenova/all-MiniLM-L6-v2",
  revision: "751bff37182d3f1213fa05d7196b954e230abad9",
  dtype: "q8",
  dims: 384,
};

/** Texts per call to the model. */
const BATCH = 32;

type Pending = { resolve: (v: number[][]) => void; reject: (e: Error) => void };
type Job = Pending & { texts: string[] };

/** Two queues: shift takes from the interactive one first, so a query overtakes background work. */
export class Lanes<T> {
  private readonly interactive: T[] = [];
  private readonly background: T[] = [];

  push(item: T, background: boolean): void {
    (background ? this.background : this.interactive).push(item);
  }

  shift(): T | undefined {
    return this.interactive.shift() ?? this.background.shift();
  }

  /** Empties both queues, returning what was in them. */
  drain(): T[] {
    return [...this.interactive.splice(0), ...this.background.splice(0)];
  }
}

/**
 * The local model, in a worker thread. A worker that failed is replaced on the next ready(). The
 * worker gets one batch at a time, interactive ones first.
 */
export class WorkerEmbedder implements LocalEmbedder {
  readonly info: EmbedderInfo;
  private worker!: Worker;
  private loaded!: Promise<void>;
  private readonly pending = new Map<number, Pending>();
  private readonly queue = new Lanes<Job>();
  private busy = false;
  private nextId = 1;
  private failure?: Error;

  /**
   * cacheDir holds the model files; offline means they're a release's bundled copy, so a missing
   * file is an error rather than a download.
   */
  constructor(
    private readonly cacheDir: string,
    private readonly model = DEFAULT_MODEL,
    private readonly offline = false,
  ) {
    this.info = {
      provider: "transformers.js",
      model: `${model.id}@${model.revision}:${model.dtype}`,
      dims: model.dims,
    };
    this.start();
  }

  private start(): void {
    this.failure = undefined;
    const { cacheDir, model, offline } = this;
    const worker = new Worker(new URL("./embedworker.js", import.meta.url), {
      workerData: { cacheDir, model: model.id, revision: model.revision, dtype: model.dtype, offline },
    });
    this.worker = worker;
    this.loaded = new Promise((resolve, reject) => {
      worker.on("message", (m: { type: string; id?: number; vectors?: number[][]; message?: string }) => {
        if (m.type === "ready") return resolve();
        if (m.type === "load-error") return this.fail(worker, new Error(m.message), reject);
        const p = this.pending.get(m.id!);
        if (!p) return;
        this.pending.delete(m.id!);
        if (m.type === "vectors") p.resolve(m.vectors!);
        else p.reject(new Error(m.message));
      });
      worker.on("error", (e) => this.fail(worker, e, reject));
      worker.on("exit", (code) => this.fail(worker, new Error(`the embedding worker exited (${code})`), reject));
    });
    this.loaded.catch(() => undefined); // read through ready()
  }

  private fail(worker: Worker, e: Error, reject: (e: Error) => void): void {
    reject(e);
    if (worker !== this.worker) return; // a replaced worker's exit
    this.failure ??= e;
    for (const p of [...this.pending.values(), ...this.queue.drain()]) p.reject(this.failure);
    this.pending.clear();
    this.busy = false;
  }

  /** Waits for the model, first replacing a worker that failed (a download that failed offline). */
  ready(): Promise<void> {
    if (this.failure) {
      void this.worker.terminate();
      this.start();
    }
    return this.loaded;
  }

  async embed(text: string): Promise<number[]> {
    return (await this.embedBatch([text]))[0];
  }

  embedBatch(texts: string[]): Promise<number[][]> {
    return this.embedMany(texts, false);
  }

  embedBackground(texts: string[]): Promise<number[][]> {
    return this.embedMany(texts, true);
  }

  private async embedMany(texts: string[], background: boolean): Promise<number[][]> {
    await this.ready();
    const out: number[][] = [];
    for (let i = 0; i < texts.length; i += BATCH) {
      out.push(...(await this.call(texts.slice(i, i + BATCH), background)));
    }
    return out;
  }

  private call(texts: string[], background: boolean): Promise<number[][]> {
    if (this.failure) return Promise.reject(this.failure);
    return new Promise((resolve, reject) => {
      this.queue.push({ texts, resolve, reject }, background);
      this.pump();
    });
  }

  /** Sends the worker the next batch, interactive first, when it has none. */
  private pump(): void {
    if (this.busy) return;
    const job = this.queue.shift();
    if (!job) return;
    this.busy = true;
    const id = this.nextId++;
    const done = () => {
      this.busy = false;
      this.pump();
    };
    this.pending.set(id, {
      resolve: (v) => {
        job.resolve(v);
        done();
      },
      reject: (e) => {
        job.reject(e);
        done();
      },
    });
    this.worker.postMessage({ id, texts: job.texts });
  }

  async close(): Promise<void> {
    await this.worker.terminate();
  }
}

/**
 * For tests only (MEKO_CONTEXT_ENGINE_TEST_EMBEDDER=hash): each word hashed into one of 384 buckets, then
 * normalized. Texts sharing words are close, which is enough to test the plumbing without a model.
 */
export class HashEmbedder implements LocalEmbedder {
  readonly info: EmbedderInfo = { provider: "test", model: "hash", dims: 384 };

  async ready(): Promise<void> {}

  async embed(text: string): Promise<number[]> {
    const v = new Array<number>(this.info.dims).fill(0);
    for (const w of text.toLowerCase().match(/[\p{L}\p{N}_]+/gu) ?? []) {
      v[createHash("sha256").update(w).digest().readUInt32LE(0) % this.info.dims] += 1;
    }
    const n = Math.hypot(...v);
    if (n === 0) v[0] = 1; // no words: any unit vector
    return v.map((x) => x / (n || 1));
  }

  async embedBatch(texts: string[]): Promise<number[][]> {
    return Promise.all(texts.map((t) => this.embed(t)));
  }

  embedBackground(texts: string[]): Promise<number[][]> {
    return this.embedBatch(texts);
  }

  async close(): Promise<void> {}
}
