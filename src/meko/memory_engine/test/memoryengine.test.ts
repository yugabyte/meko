process.env.MEM0_TELEMETRY = "false";

import assert from "node:assert/strict";
import fs from "node:fs";
import http from "node:http";
import os from "node:os";
import path from "node:path";
import { after, before, describe, test } from "node:test";

import { EmbedderFactory, HistoryManagerFactory, LLMFactory, VectorStoreFactory } from "mem0ai/oss";

import { loadReranker, startUp } from "../src/app.js";
import { DEFAULT_MODEL, HashEmbedder, Lanes, WorkerEmbedder } from "../src/embedder.js";
import { MemoryEngine, HttpError, toResult } from "../src/memoryengine.js";
import { MekoHistory, MekoStore, NoLLM, PROVIDERS, register } from "../src/providers.js";
import { DEFAULT_RERANKER, type LocalReranker, OverlapReranker, WorkerReranker } from "../src/reranker.js";
import { mekoScore } from "../src/scoring.js";
import { startServer, type Health } from "../src/server.js";
import { StoreClient, StoreError } from "../src/storeclient.js";
import { startFakeStore, type FakeStore } from "./fakestore.js";

describe("mekoScore", () => {
  test("divides each memory only by the signals it earned", () => {
    assert.equal(mekoScore({ semanticScore: 0.8, bm25Score: 0, entityBoost: 0 }), 0.8);
    assert.equal(mekoScore({ semanticScore: 0.6, bm25Score: 0.9, entityBoost: 0 }), 0.75);
    assert.equal(mekoScore({ semanticScore: 0.6, bm25Score: 0, entityBoost: 0.3 }), 0.6);
    assert.ok(Math.abs(mekoScore({ semanticScore: 0.5, bm25Score: 0.5, entityBoost: 0.25 }) - 0.5) < 1e-12);
  });
  test("caps at 1", () => {
    assert.equal(mekoScore({ semanticScore: 1, bm25Score: 1, entityBoost: 0.5 }), 1);
  });
});

describe("WorkerEmbedder", () => {
  test("offline (a release's bundled models) never downloads: a missing model is an error", async () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "meko-models-"));
    const e = new WorkerEmbedder(dir, DEFAULT_MODEL, true);
    try {
      await assert.rejects(e.ready(), /can't load the embedding model Xenova\/all-MiniLM-L6-v2/);
      assert.deepEqual(fs.readdirSync(dir), [], "nothing was fetched into the models folder");
    } finally {
      await e.close();
      fs.rmSync(dir, { recursive: true, force: true });
    }
  });
});

describe("WorkerReranker", () => {
  test("offline (a release's bundled models) never downloads: a missing model is an error", async () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "meko-models-"));
    const r = new WorkerReranker(dir, DEFAULT_RERANKER, true);
    try {
      await assert.rejects(r.ready(), /can't load the reranking model Xenova\/ms-marco-MiniLM-L-6-v2/);
      assert.deepEqual(fs.readdirSync(dir), [], "nothing was fetched into the models folder");
    } finally {
      await r.close();
      fs.rmSync(dir, { recursive: true, force: true });
    }
  });
});

describe("toResult", () => {
  test("is prod's shape: snake_case, ids lifted, tenant ids in metadata", () => {
    const r = toResult("dp1", {
      id: "m1",
      memory: "text",
      hash: "h",
      createdAt: "2026-10-01T00:00:00.000Z",
      updatedAt: "2026-10-02T00:00:00.000Z",
      user_id: "u",
      agent_id: "a",
      attributedTo: "user",
      metadata: { role: "user", actor_id: "hari", meko_conversation_id: "c1", custom: 1 },
    });
    assert.deepEqual(r, {
      id: "m1",
      memory: "text",
      hash: "h",
      metadata: { meko_conversation_id: "c1", custom: 1, meko_datapack_id: "dp1", meko_user_id: "u", meko_agent_id: "a" },
      created_at: "2026-10-01T00:00:00.000Z",
      updated_at: "2026-10-02T00:00:00.000Z",
      user_id: "u",
      agent_id: "a",
      run_id: "c1",
      role: "user",
      actor_id: "hari",
      attributed_to: "user",
    });
  });
});

describe("with a store", () => {
  let fake: FakeStore;
  let memoryEngine: MemoryEngine;
  const embedder = new HashEmbedder();

  before(async () => {
    fake = await startFakeStore();
    register(new StoreClient(fake.socket, fake.token), embedder);
    memoryEngine = new MemoryEngine(embedder);
  });
  after(() => fake.close());

  test("mem0's factories build Meko's providers, and still build their own", () => {
    assert.ok(VectorStoreFactory.create(PROVIDERS.store, { collectionName: "dp" }) instanceof MekoStore);
    assert.ok(HistoryManagerFactory.create(PROVIDERS.history, { provider: "x", config: { datapackId: "dp" } } as any) instanceof
      MekoHistory);
    assert.equal(EmbedderFactory.create(PROVIDERS.embedder, {}), embedder);
    assert.ok(LLMFactory.create(PROVIDERS.llm, {}) instanceof NoLLM);
    assert.equal(LLMFactory.create("openai", { apiKey: "unused" }).constructor.name, "OpenAILLM");
  });

  test("the store client sends mem0's calls to the right routes", async () => {
    const s = new MekoStore(new StoreClient(fake.socket, fake.token), "dp x_entities");
    fake.requests.length = 0;
    await s.insert([[1, 0]], ["e1"], [{ data: "AgentK" }]);
    assert.deepEqual(await s.get("e1"), { id: "e1", payload: { data: "AgentK" } });
    assert.equal(await s.get("nope"), null);
    const [rows, total] = await s.list({ data: "AgentK" }, 5);
    assert.equal(total, 1);
    assert.equal(rows[0].id, "e1");
    await s.delete("e1");
    await assert.rejects(s.deleteCol());
    assert.deepEqual(
      fake.requests.map((r) => `${r.method} ${r.path}`),
      [
        "POST /v1/collections/dp%20x_entities/insert",
        "GET /v1/collections/dp%20x_entities/vectors/e1",
        "GET /v1/collections/dp%20x_entities/vectors/nope",
        "POST /v1/collections/dp%20x_entities/list",
        "DELETE /v1/collections/dp%20x_entities/vectors/e1",
      ],
    );
  });

  test("adds verbatim, with role, actor and conversation, and no entities", async () => {
    const r = await memoryEngine.add({
      datapack_id: "dp1",
      user_id: "u",
      agent_id: "claude_code:x",
      conversation_id: "conv1",
      messages: [
        { role: "system", content: "skipped" },
        { role: "user", content: "AgentK sessions survive server restarts." },
        { role: "assistant", content: "Noted.", name: "helper" },
        { role: "user", content: "An empty name is no actor.", name: "" },
      ],
      // Only a message's name sets the actor.
      metadata: { source_note: "test", actor_id: "claimed", meko_actor_id: "claimed" },
    });
    assert.deepEqual(
      r.results.map((x) => [x.memory, x.event, x.role, x.actor_id]),
      [
        ["AgentK sessions survive server restarts.", "ADD", "user", null],
        ["Noted.", "ADD", "assistant", "helper"],
        ["An empty name is no actor.", "ADD", "user", null],
      ],
    );
    const rows = [...fake.rows.get("dp1")!.values()].map((x) => x.payload);
    assert.equal(rows[0].role, "user");
    assert.equal(rows[0].meko_conversation_id, "conv1");
    assert.equal(rows[0].user_id, "u");
    assert.equal(rows[0].agent_id, "claude_code:x");
    assert.equal(rows[0].source_note, "test");
    assert.equal(rows[0].actor_id, undefined);
    assert.equal(rows[1].actor_id, "helper");
    assert.equal(rows[2].actor_id, undefined);
    assert.ok(rows.every((p) => !("meko_actor_id" in p)));
    assert.equal(fake.rows.get("dp1_entities")?.size ?? 0, 0);
    assert.equal(fake.history.filter((h) => h.action === "ADD").length, 3);
  });

  test("refuses a message that isn't {role, content} strings", async () => {
    for (const messages of [
      [{ role: "user", content: [{ type: "text", text: "x" }] }],
      [null],
      ["x"],
      [{ role: "user", content: "x", name: 7 }],
    ]) {
      await assert.rejects(
        memoryEngine.add({ datapack_id: "dp1", user_id: "u", messages: messages as any }),
        (e: HttpError) => e.status === 400,
      );
    }
  });

  test("refuses extraction", async () => {
    await assert.rejects(
      memoryEngine.add({ datapack_id: "dp1", user_id: "u", messages: [{ role: "user", content: "x" }], infer: true }),
      (e: HttpError) => e.code === "INFER_UNAVAILABLE",
    );
  });

  test("search scores the Meko way and returns prod's shape", async () => {
    await memoryEngine.add({
      datapack_id: "dp2",
      user_id: "u",
      messages: [
        { role: "user", content: "the rate limit is five hundred requests" },
        { role: "user", content: "bananas are yellow" },
      ],
    });
    const { results } = await memoryEngine.search({ datapack_id: "dp2", user_id: "u", query: "rate limit", limit: 5 });
    assert.equal(results[0].memory, "the rate limit is five hundred requests");
    for (const r of results) {
      assert.ok(r.score! >= 0 && r.score! <= 1, `score ${r.score}`);
      assert.ok(r.raw_similarity_score! >= 0.1);
      assert.equal(r.metadata.meko_datapack_id, "dp2");
    }
    // The keyword hit counts toward the top result's divisor, not the other one's.
    const top = results[0];
    assert.ok(top.score! > top.raw_similarity_score! / 2);
    const other = await memoryEngine.search({ datapack_id: "dp2", user_id: "other", query: "rate limit" });
    assert.equal(other.results.length, 0, "another user's memories");
    await assert.rejects(
      memoryEngine.search({ datapack_id: "dp2", user_id: "u", query: "rate", limit: "abc" as any }),
      (e: HttpError) => e.status === 400,
    );
  });

  test("search rescores all of mem0's candidates: mem0 cuts none", async () => {
    fake.requests.length = 0;
    await memoryEngine.search({ datapack_id: "dp2", user_id: "u", query: "rate limit", limit: 10 });
    const asked = fake.requests.filter((r) => r.path.endsWith("search")).map((r) => [r.path, r.body.top_k]);
    assert.deepEqual(asked, [
      ["/v1/collections/dp2/search", 240],
      ["/v1/collections/dp2/keyword-search", 240],
      ["/v1/collections/dp2_entities/search", 500], // entities are searched as mem0 asks
    ]);
  });

  test("update and delete go through mem0, and a missing id is a 404", async () => {
    const { results } = await memoryEngine.add({ datapack_id: "dp3", user_id: "u", messages: [{ role: "user", content: "old" }] });
    const id = results[0].id as string;
    await memoryEngine.update({ datapack_id: "dp3", memory_id: id, text: "new text" });
    assert.equal(fake.rows.get("dp3")!.get(id)!.payload.data, "new text");
    await memoryEngine.delete({ datapack_id: "dp3", memory_id: id });
    assert.equal(fake.rows.get("dp3")!.has(id), false);
    await assert.rejects(memoryEngine.delete({ datapack_id: "dp3", memory_id: id }), (e: HttpError) => e.status === 404);
  });

  test("embeds in batches, within the limit", async () => {
    const { vectors } = await memoryEngine.embed({ texts: ["a", "b"] });
    assert.equal(vectors.length, 2);
    assert.equal(vectors[0].length, 384);
    await assert.rejects(memoryEngine.embed({ texts: new Array(257).fill("x") }), (e: HttpError) => e.status === 400);
  });

  test("embeds in the background on request, and refuses a background that isn't a boolean", async () => {
    const { vectors } = await memoryEngine.embed({ texts: ["a"], background: true });
    assert.deepEqual(vectors, await embedder.embedBatch(["a"]));
    await assert.rejects(
      memoryEngine.embed({ texts: ["a"], background: "yes" as unknown as boolean }),
      (e: HttpError) => e.status === 400,
    );
  });

  test("reranks with its reranker, and says when it has none ready", async () => {
    const ranked = new MemoryEngine(embedder, 2, new OverlapReranker());
    const { scores } = await ranked.rerank({ query: "deploy the server", texts: ["how to deploy the server", "lunch"] });
    assert.equal(scores.length, 2);
    assert.ok(scores[0] > scores[1]);
    assert.deepEqual(await ranked.rerank({ query: "x", texts: [] }), { scores: [] });
    for (const bad of [
      { query: "", texts: ["a"] },
      { query: "q", texts: [3] },
      { query: "q", texts: new Array(201).fill("x") },
    ]) {
      await assert.rejects(ranked.rerank(bad as any), (e: HttpError) => e.status === 400, JSON.stringify(bad).slice(0, 40));
    }
    let revived = 0;
    const loading: LocalReranker = {
      info: { provider: "test", model: "m" },
      state: "loading",
      ready: async () => {},
      revive: () => void revived++,
      rerank: async () => [],
      close: async () => {},
    };
    const failed: LocalReranker = { ...loading, state: "error", error: "the worker exited (1)" };
    for (const e of [new MemoryEngine(embedder), new MemoryEngine(embedder, 2, loading)]) {
      await assert.rejects(e.rerank({ query: "q", texts: ["a"] }), (err: HttpError) => err.code === "RERANKER_NOT_READY");
    }
    assert.equal(revived, 0, "a reranker still loading isn't revived");
    // A reranker whose worker died is revived, and this call says why it's not ready.
    await assert.rejects(new MemoryEngine(embedder, 2, failed).rerank({ query: "q", texts: ["a"] }),
      (err: HttpError) => err.code === "RERANKER_NOT_READY" && /exited/.test(err.message));
    assert.equal(revived, 1);
  });

  test("keeps the most recently used datapacks' Memory, and rebuilds a dropped one", async () => {
    const small = new MemoryEngine(embedder, 2);
    const add = (dp: string) =>
      small.add({ datapack_id: dp, user_id: "u", agent_id: "a", messages: [{ role: "user", content: `lru ${dp}` }] });
    await add("lru-a");
    await add("lru-b");
    await small.search({ datapack_id: "lru-a", user_id: "u", query: "lru" }); // a is now newer than b
    await add("lru-c");
    assert.equal(small.datapacksKept, 2);
    // b was dropped; its memories are in the store, so its next call finds them.
    const { results } = await small.search({ datapack_id: "lru-b", user_id: "u", query: "lru lru-b" });
    assert.equal(results[0]?.memory, "lru lru-b");
    assert.equal(small.datapacksKept, 2);
  });
});

describe("startUp", () => {
  const info = { provider: "test", model: "m", dims: 2 };

  test("retries a failed load or hello until it passes", async () => {
    let loads = 0;
    let hellos = 0;
    const health: Health = { state: "loading", embedder: info };
    const embedder = {
      info,
      ready: async () => {
        if (++loads === 1) throw new Error("offline");
      },
    };
    await startUp(
      embedder,
      async () => {
        if (++hellos === 1) throw new StoreError(500, "INTERNAL_ERROR", "busy");
      },
      health,
      1,
    );
    assert.deepEqual([loads, hellos, health.state, health.error], [3, 2, "ready", undefined]);
  });

  test("stops at another model in the store, saying why", async () => {
    const health: Health = { state: "loading", embedder: info };
    await startUp(
      { info, ready: async () => {} },
      async () => {
        throw new StoreError(409, "MODEL_CHANGED", "the store holds vectors from another model");
      },
      health,
      1,
    );
    assert.equal(health.state, "error");
    assert.match(health.error!, /another model/);
  });
});

describe("the embedder's queues", () => {
  test("an interactive batch overtakes background ones", () => {
    const q = new Lanes<string>();
    q.push("bg1", true);
    q.push("bg2", true);
    q.push("query", false);
    assert.deepEqual([q.shift(), q.shift()], ["query", "bg1"]);
    q.push("query2", false);
    assert.deepEqual(q.drain(), ["query2", "bg2"]);
    assert.equal(q.shift(), undefined);
  });
});

describe("loadReranker", () => {
  test("retries a failed load until it passes", async () => {
    let loads = 0;
    await loadReranker(
      {
        ready: async () => {
          if (++loads < 3) throw new Error("offline");
        },
      },
      1,
    );
    assert.equal(loads, 3);
  });
});

describe("the API", () => {
  let fake: FakeStore;
  let server: http.Server;
  let socket: string;

  before(async () => {
    fake = await startFakeStore();
    const embedder = new HashEmbedder();
    register(new StoreClient(fake.socket, fake.token), embedder);
    socket = path.join(fs.mkdtempSync(path.join(os.tmpdir(), "mk-")), "mem_eng.sock");
    const health: Health = { state: "ready", embedder: embedder.info };
    server = await startServer(socket, "tok", new MemoryEngine(embedder), () => health, () => {});
  });
  after(async () => {
    server.close();
    await fake.close();
  });

  const post = (route: string, body: string | Buffer) =>
    new Promise<{ status: number; body: any }>((resolve, reject) => {
      const req = http.request(
        { socketPath: socket, path: route, method: "POST", headers: { authorization: "Bearer tok" } },
        (res) => {
          const chunks: Buffer[] = [];
          res.on("data", (c) => chunks.push(c));
          res.on("end", () => resolve({ status: res.statusCode!, body: JSON.parse(Buffer.concat(chunks).toString()) }));
        },
      );
      req.on("error", reject);
      req.end(body);
    });

  test("a body that isn't a JSON object is a 400", async () => {
    for (const body of ["null", "[]", "3", "{"]) {
      const r = await post("/v1/search", body);
      assert.equal(r.status, 400, body);
      assert.equal(r.body.error.code, "BAD_REQUEST");
    }
  });

  test("reranks on /v1/rerank", async () => {
    const r = await post("/v1/rerank", JSON.stringify({ query: "q", texts: ["a"] }));
    assert.equal(r.status, 503);
    assert.equal(r.body.error.code, "RERANKER_NOT_READY");
  });

  test("a body over the limit gets its 413", async () => {
    const r = await post("/v1/embed", Buffer.alloc(9 << 20, 0x20));
    assert.equal(r.status, 413);
    assert.equal(r.body.error.code, "PAYLOAD_TOO_LARGE");
  });
});
