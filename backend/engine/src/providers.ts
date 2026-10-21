// Meko's providers for mem0: a vector store and a history store over the Go server's store
// API, the embedder, and an LLM that refuses (no extraction in this milestone). mem0 builds its
// providers by name in fixed switch statements, so register() wraps the exported factories'
// create methods; mem0 looks them up at call time, so this needs no fork or patch.
import {
  EmbedderFactory,
  HistoryManagerFactory,
  LLMFactory,
  VectorStoreFactory,
  type Embedder,
  type LLM,
  type LLMResponse,
  type SearchFilters,
  type VectorStore,
  type VectorStoreResult,
} from "mem0ai/oss";

import type { StoreClient } from "./storeclient.js";

/** Provider names Meko registers. */
export const PROVIDERS = { store: "meko_store", history: "meko_history", embedder: "meko_embedder", llm: "none" };

const enc = encodeURIComponent;

/**
 * mem0's Memory.search (3.3.1) asks the store for max(topK × 4, 60) semantic and keyword candidates,
 * ranks them by its own score and keeps topK. The engine rescores mem0's results, so a datapack's
 * collection returns only topK / 4 of what it's asked for: then mem0's candidates are exactly the
 * topK it returns, it cuts none, and Meko's score picks from all of them. The entity collection is
 * searched as asked. (Only Memory.search searches a datapack's collection while extraction is off.)
 */
export const MEM0_OVERFETCH = 4;

/**
 * mem0's Memory.add (3.3) drops actor_id from the metadata it's given, as it drops the user, agent
 * and run ids, and never sets it itself. So the engine passes a message's speaker under this key and
 * MekoStore.insert stores it as actor_id, where the store, mem0's search and the Go server read it.
 */
export const ACTOR_KEY = "meko_actor_id";

/** A mem0 vector store whose collection is a datapack ("<id>") or its entities ("<id>_entities"). */
export class MekoStore implements VectorStore {
  private readonly base: string;
  private readonly memories: boolean;

  constructor(
    private readonly store: StoreClient,
    collectionName: string,
  ) {
    this.base = `/v1/collections/${enc(collectionName)}`;
    this.memories = !collectionName.endsWith("_entities");
  }

  /** How many candidates to fetch when mem0 asks for topK: see MEM0_OVERFETCH. */
  private candidates(topK: number): number {
    return this.memories ? Math.ceil(topK / MEM0_OVERFETCH) : topK;
  }

  async insert(vectors: number[][], ids: string[], payloads: Record<string, any>[]): Promise<void> {
    await this.store.request("POST", `${this.base}/insert`, { ids, vectors, payloads: payloads.map(withActor) });
  }

  async search(query: number[], topK = 10, filters?: SearchFilters): Promise<VectorStoreResult[]> {
    const r = await this.store.request<{ results: VectorStoreResult[] }>("POST", `${this.base}/search`, {
      vector: query,
      top_k: this.candidates(topK),
      filters: filters ?? {},
    });
    return r.results;
  }

  async keywordSearch(query: string, topK = 10, filters?: SearchFilters): Promise<VectorStoreResult[] | null> {
    const r = await this.store.request<{ results: VectorStoreResult[] }>("POST", `${this.base}/keyword-search`, {
      query,
      top_k: this.candidates(topK),
      filters: filters ?? {},
    });
    return r.results;
  }

  async get(vectorId: string): Promise<VectorStoreResult | null> {
    return this.store.request<VectorStoreResult | null>("GET", `${this.base}/vectors/${enc(vectorId)}`);
  }

  async update(vectorId: string, vector: number[], payload: Record<string, any>): Promise<void> {
    await this.store.request("PUT", `${this.base}/vectors/${enc(vectorId)}`, { vector, payload });
  }

  async delete(vectorId: string): Promise<void> {
    await this.store.request("DELETE", `${this.base}/vectors/${enc(vectorId)}`);
  }

  async deleteCol(): Promise<void> {
    throw new Error("Meko's engine never drops a datapack's collection");
  }

  async list(filters?: SearchFilters, topK?: number): Promise<[VectorStoreResult[], number]> {
    const r = await this.store.request<{ results: VectorStoreResult[]; total: number }>("POST", `${this.base}/list`, {
      filters: filters ?? {},
      top_k: topK ?? 0,
    });
    return [r.results, r.total];
  }

  // Only mem0's telemetry uses these.
  async getUserId(): Promise<string> {
    return "meko";
  }

  async setUserId(): Promise<void> {}

  async initialize(): Promise<void> {}
}

/** A payload with the speaker the engine passed under ACTOR_KEY as actor_id: see ACTOR_KEY. */
function withActor(payload: Record<string, any>): Record<string, any> {
  if (!(ACTOR_KEY in payload)) return payload;
  const { [ACTOR_KEY]: actor, ...rest } = payload;
  return typeof actor === "string" && actor !== "" ? { ...rest, actor_id: actor } : rest;
}

interface HistoryRecord {
  memoryId: string;
  previousValue: string | null;
  newValue: string | null;
  action: string;
  createdAt?: string;
  updatedAt?: string;
  isDeleted?: number;
}

/**
 * mem0's history of changes to memories, kept per datapack by the Go server. (mem0 doesn't export
 * its HistoryManager interface; this class matches it.)
 */
export class MekoHistory {
  constructor(
    private readonly store: StoreClient,
    private readonly datapackId: string,
  ) {}

  async addHistory(
    memoryId: string,
    previousValue: string | null,
    newValue: string | null,
    action: string,
    createdAt?: string,
    updatedAt?: string,
    isDeleted?: number,
  ): Promise<void> {
    await this.batchAddHistory([{ memoryId, previousValue, newValue, action, createdAt, updatedAt, isDeleted }]);
  }

  async batchAddHistory(records: HistoryRecord[]): Promise<void> {
    const now = new Date().toISOString();
    await this.store.request("POST", `/v1/history/${enc(this.datapackId)}`, {
      entries: records.map((r) => ({
        memory_id: r.memoryId,
        previous_value: r.previousValue ?? null,
        new_value: r.newValue ?? null,
        action: r.action,
        created_at: r.createdAt ?? now,
        updated_at: r.updatedAt ?? null,
        is_deleted: r.isDeleted === 1,
      })),
    });
  }

  async getHistory(memoryId: string): Promise<any[]> {
    return this.store.request<any[]>("GET", `/v1/history/${enc(this.datapackId)}/${enc(memoryId)}`);
  }

  async reset(): Promise<void> {
    throw new Error("Meko's engine never resets memory history");
  }

  close(): void {}

  // Extraction reads earlier turns from the Go server's messages; it comes in a later milestone.
  async saveMessages(): Promise<void> {}

  async getLastMessages(): Promise<Array<{ role: string; content: string; createdAt: string }>> {
    return [];
  }
}

/** The LLM until extraction exists: mem0 always builds one, and this one is never meant to run. */
export class NoLLM implements LLM {
  async generateResponse(): Promise<any> {
    throw new Error("extraction isn't available in Meko yet");
  }

  async generateChat(): Promise<LLMResponse> {
    throw new Error("extraction isn't available in Meko yet");
  }
}

let registered = false;

/** Makes mem0's factories build Meko's providers. Other names still reach mem0's own. */
export function register(store: StoreClient, embedder: Embedder): void {
  if (registered) return;
  registered = true;
  const vectorStore = VectorStoreFactory.create.bind(VectorStoreFactory);
  VectorStoreFactory.create = (provider: string, config: any) =>
    provider === PROVIDERS.store ? new MekoStore(store, config.collectionName) : vectorStore(provider, config);
  const history = HistoryManagerFactory.create.bind(HistoryManagerFactory);
  HistoryManagerFactory.create = (provider: string, config: any) =>
    provider === PROVIDERS.history ? new MekoHistory(store, config.config.datapackId) : history(provider, config);
  const embed = EmbedderFactory.create.bind(EmbedderFactory);
  EmbedderFactory.create = (provider: string, config: any) =>
    provider === PROVIDERS.embedder ? embedder : embed(provider, config);
  const llm = LLMFactory.create.bind(LLMFactory);
  LLMFactory.create = (provider: string, config: any) => (provider === PROVIDERS.llm ? new NoLLM() : llm(provider, config));
}
