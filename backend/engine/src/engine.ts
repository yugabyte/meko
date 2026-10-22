// The engine's memory operations: one mem0 Memory per datapack, with Meko's providers, and
// the reply shapes prod Meko gives (snake_case keys, the event at the top level, ids lifted out of
// metadata, the score Meko computes and the cosine as raw_similarity_score).
import { Memory } from "mem0ai/oss";

import type { LocalEmbedder } from "./embedder.js";
import { ACTOR_KEY, MEM0_OVERFETCH, PROVIDERS } from "./providers.js";
import type { LocalReranker } from "./reranker.js";
import { mekoScore, type ScoreDetails } from "./scoring.js";

/** A failure with an HTTP status and a code, for the engine API to reply with. */
export class HttpError extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
    message: string,
  ) {
    super(message);
  }
}

export interface Message {
  role: string;
  content: string;
  name?: string;
}

export interface AddRequest {
  datapack_id: string;
  user_id: string;
  agent_id?: string;
  conversation_id?: string;
  messages: Message[];
  metadata?: Record<string, unknown>;
  infer?: boolean;
}

export interface SearchRequest {
  datapack_id: string;
  user_id: string;
  agent_id?: string;
  conversation_id?: string;
  query: string;
  limit?: number;
}

export interface MemoryResult {
  id: string;
  memory: string;
  hash?: string;
  metadata: Record<string, unknown>;
  score?: number;
  raw_similarity_score?: number;
  created_at?: string;
  updated_at?: string;
  user_id?: string;
  agent_id?: string;
  run_id?: string;
  actor_id?: string;
  role?: string;
  attributed_to?: string;
  expiration_date?: string;
}

/** Most results one search returns. */
export const MAX_LIMIT = 100;
/** Fewest memories a search rescores: the nearest by cosine, and as many by keyword. */
export const MIN_CANDIDATES = 240;
/** Most texts one embed call takes. */
export const MAX_EMBED = 256;
/** Most texts one rerank call takes: a knowledge-base search reranks up to twice its pool. */
export const MAX_RERANK = 200;
/**
 * Most datapacks whose mem0 Memory is kept. The least recently used is dropped past it, so a
 * deleted datapack's (which no call reaches again) doesn't stay until the engine restarts. A
 * Memory holds no data of its own: a dropped one is rebuilt on its datapack's next call.
 */
export const MAX_DATAPACKS = 256;

export class Engine {
  private readonly memories = new Map<string, Memory>();

  constructor(
    private readonly embedder: LocalEmbedder,
    private readonly maxDatapacks = MAX_DATAPACKS,
    private readonly reranker?: LocalReranker,
  ) {}

  /** How many datapacks' Memory objects are kept. */
  get datapacksKept(): number {
    return this.memories.size;
  }

  private memory(datapackId: string): Memory {
    let m = this.memories.get(datapackId);
    if (m) {
      // Most recently used last: a Map iterates in insertion order.
      this.memories.delete(datapackId);
      this.memories.set(datapackId, m);
    } else {
      m = new Memory({
        version: "v1.1",
        embedder: { provider: PROVIDERS.embedder, config: { embeddingDims: this.embedder.info.dims } },
        vectorStore: {
          provider: PROVIDERS.store,
          config: { collectionName: datapackId, dimension: this.embedder.info.dims },
        },
        llm: { provider: PROVIDERS.llm, config: {} },
        // mem0's type knows only its own stores' keys; MekoHistory reads datapackId.
        historyStore: { provider: PROVIDERS.history, config: { datapackId } as Record<string, string> },
      });
      this.memories.set(datapackId, m);
      if (this.memories.size > this.maxDatapacks) {
        this.memories.delete(this.memories.keys().next().value!);
      }
    }
    return m;
  }

  /**
   * Stores each non-system message as a memory, verbatim, as prod's memory_add does: with its role,
   * the speaker's name as actor_id, and the conversation it came from. No entities are linked
   * (mem0 links them only for extracted memories). System messages and blank ones are left out,
   * as mem0 leaves them out; a message that isn't {role, content} strings, or whose name isn't a
   * string, refuses the request.
   * The speaker travels under ACTOR_KEY, set from the message alone so metadata can't claim one.
   */
  async add(req: AddRequest): Promise<{ results: Array<Record<string, unknown>> }> {
    requireIds(req);
    if (req.infer) {
      throw new HttpError(400, "INFER_UNAVAILABLE", "extracting memories isn't available in Meko yet");
    }
    if (!Array.isArray(req.messages) || req.messages.length === 0) {
      throw new HttpError(400, "BAD_REQUEST", "messages must be a non-empty list");
    }
    for (const m of req.messages) {
      if (typeof m !== "object" || m === null || typeof m.role !== "string" || typeof m.content !== "string") {
        throw new HttpError(400, "BAD_REQUEST", "each message must have a role and a content string");
      }
      if (m.name !== undefined && typeof m.name !== "string") {
        throw new HttpError(400, "BAD_REQUEST", "a message's name must be a string");
      }
    }
    const mem = this.memory(req.datapack_id);
    const results: Array<Record<string, unknown>> = [];
    for (const m of req.messages) {
      if (m.role === "system" || m.content.trim() === "") continue;
      const metadata: Record<string, unknown> = {
        ...(req.metadata ?? {}),
        role: m.role,
        [ACTOR_KEY]: m.name || undefined,
        ...(req.conversation_id && { meko_conversation_id: req.conversation_id }),
      };
      const r = await mem.add(m.content, {
        userId: req.user_id,
        ...(req.agent_id && { agentId: req.agent_id }),
        metadata,
        infer: false,
      });
      for (const x of r.results) {
        results.push({ id: x.id, memory: x.memory, event: "ADD", actor_id: m.name || null, role: m.role });
      }
    }
    return { results };
  }

  /**
   * mem0's hybrid search (semantic candidates, BM25 and entity boosts), rescored the way Meko
   * scores: each memory divided by what it earned. mem0 is asked for as many results as it has
   * candidates (the store returns topK of the 4 × topK mem0 asks it for: see MEM0_OVERFETCH), so
   * mem0 cuts nothing and the rescoring, not upstream's ranking, picks the top results.
   */
  async search(req: SearchRequest): Promise<{ results: MemoryResult[] }> {
    requireIds(req);
    if (typeof req.query !== "string" || req.query.trim() === "") {
      throw new HttpError(400, "BAD_REQUEST", "query must be a non-empty string");
    }
    if (req.limit !== undefined && (typeof req.limit !== "number" || !Number.isFinite(req.limit))) {
      throw new HttpError(400, "BAD_REQUEST", "limit must be a number");
    }
    const limit = Math.min(Math.max(Math.trunc(req.limit ?? 10), 1), MAX_LIMIT);
    const filters: Record<string, string> = { user_id: req.user_id };
    if (req.agent_id) filters.agent_id = req.agent_id;
    if (req.conversation_id) filters.meko_conversation_id = req.conversation_id;
    const r = await this.memory(req.datapack_id).search(req.query, {
      topK: Math.max(limit * MEM0_OVERFETCH * 4, MIN_CANDIDATES),
      filters,
      explain: true,
    });
    const results = (r.results as Array<Record<string, any>>).map((x) => {
      const d = x.score_details as ScoreDetails;
      return {
        ...toResult(req.datapack_id, x),
        score: mekoScore(d),
        raw_similarity_score: d.semanticScore,
      };
    });
    results.sort((a, b) => b.score - a.score);
    return { results: results.slice(0, limit) };
  }

  async update(req: { datapack_id: string; memory_id: string; text: string }): Promise<{ id: string }> {
    if (typeof req.text !== "string" || req.text.trim() === "") {
      throw new HttpError(400, "BAD_REQUEST", "text must be a non-empty string");
    }
    await notFoundAs404(this.memory(req.datapack_id).update(req.memory_id, { text: req.text }));
    return { id: req.memory_id };
  }

  async delete(req: { datapack_id: string; memory_id: string }): Promise<{ id: string }> {
    await notFoundAs404(this.memory(req.datapack_id).delete(req.memory_id));
    return { id: req.memory_id };
  }

  /** Embeds texts; background ones (the Go server's sweeps) wait behind searches' queries. */
  async embed(req: { texts: string[]; background?: boolean }): Promise<{ vectors: number[][] }> {
    if (!Array.isArray(req.texts) || req.texts.some((t) => typeof t !== "string")) {
      throw new HttpError(400, "BAD_REQUEST", "texts must be a list of strings");
    }
    if (req.texts.length > MAX_EMBED) {
      throw new HttpError(400, "BAD_REQUEST", `at most ${MAX_EMBED} texts per call`);
    }
    if (req.background !== undefined && typeof req.background !== "boolean") {
      throw new HttpError(400, "BAD_REQUEST", "background must be a boolean");
    }
    if (!req.texts.length) return { vectors: [] };
    const vectors = req.background
      ? await this.embedder.embedBackground(req.texts)
      : await this.embedder.embedBatch(req.texts);
    return { vectors };
  }

  /** The reranker's raw score (a logit) for each text against the query. */
  async rerank(req: { query: string; texts: string[] }): Promise<{ scores: number[] }> {
    if (typeof req.query !== "string" || req.query.trim() === "") {
      throw new HttpError(400, "BAD_REQUEST", "query must be a non-empty string");
    }
    if (!Array.isArray(req.texts) || req.texts.some((t) => typeof t !== "string")) {
      throw new HttpError(400, "BAD_REQUEST", "texts must be a list of strings");
    }
    if (req.texts.length > MAX_RERANK) {
      throw new HttpError(400, "BAD_REQUEST", `at most ${MAX_RERANK} texts per call`);
    }
    const r = this.reranker;
    if (!r || r.state !== "ready") {
      throw new HttpError(503, "RERANKER_NOT_READY", r?.error ?? "the reranking model is still loading");
    }
    return { scores: req.texts.length ? await r.rerank(req.query, req.texts) : [] };
  }
}

function requireIds(req: { datapack_id?: unknown; user_id?: unknown }): void {
  if (typeof req.datapack_id !== "string" || req.datapack_id === "") {
    throw new HttpError(400, "BAD_REQUEST", "datapack_id is required");
  }
  if (typeof req.user_id !== "string" || req.user_id === "") {
    throw new HttpError(400, "BAD_REQUEST", "user_id is required");
  }
}

async function notFoundAs404<T>(p: Promise<T>): Promise<T> {
  try {
    return await p;
  } catch (e) {
    if (/not found/i.test((e as Error).message)) throw new HttpError(404, "NOT_FOUND", "no such memory");
    throw e;
  }
}

// Keys mem0's search result keeps in metadata that prod's reply lifts to the top.
const LIFTED = ["role", "actor_id", "expiration_date"] as const;

/** A mem0 search result in prod Meko's shape. */
export function toResult(datapackId: string, x: Record<string, any>): MemoryResult {
  const metadata: Record<string, unknown> = { ...(x.metadata ?? {}) };
  const lifted: Record<string, unknown> = {};
  for (const k of LIFTED) {
    if (metadata[k] !== undefined) lifted[k] = metadata[k];
    delete metadata[k];
  }
  const conversation = metadata.meko_conversation_id as string | undefined;
  Object.assign(metadata, {
    meko_datapack_id: datapackId,
    meko_user_id: x.user_id,
    ...(x.agent_id && { meko_agent_id: x.agent_id }),
  });
  return {
    id: x.id,
    memory: x.memory,
    hash: x.hash,
    metadata,
    created_at: x.createdAt,
    updated_at: x.updatedAt,
    ...(x.user_id && { user_id: x.user_id }),
    ...(x.agent_id && { agent_id: x.agent_id }),
    ...(conversation && { run_id: conversation }),
    ...lifted,
    ...(x.attributedTo && { attributed_to: x.attributedTo }),
  };
}
