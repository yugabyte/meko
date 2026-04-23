import type { Express } from "express";
import { createServer, type Server } from "http";

type Datapack = {
  id: string;
  name: string;
  account_id: string;
  created_at: string;
  updated_at: string;
};

type Agent = {
  id: string;
  datapack_id: string;
  agent_name: string;
  agent_metadata: string;
  created_at: string;
  updated_at: string;
};

type KnowledgeBase = {
  id: string;
  datapack_id: string;
  knowledge_base_url: string;
  created_at: string;
  updated_at: string;
};

const accountId = "acc_prod_9f8e7d";
const nowIso = () => new Date().toISOString();

const datapacks: Datapack[] = [
  {
    id: "dp_default_1",
    name: "default",
    account_id: accountId,
    created_at: nowIso(),
    updated_at: nowIso(),
  },
];

const agentsByDatapack: Record<string, Agent[]> = {
  default: [
    {
      id: "agt_default_1",
      datapack_id: "dp_default_1",
      agent_name: "meko_user_agent",
      agent_metadata: "{}",
      created_at: nowIso(),
      updated_at: nowIso(),
    },
  ],
};

const kbByDatapack: Record<string, KnowledgeBase[]> = {
  default: [],
};

function ok<T>(data: T) {
  return { data, error: null };
}

export async function registerRoutes(
  httpServer: Server,
  app: Express,
): Promise<Server> {
  app.get("/api/v1/datapacks", (_req, res) => {
    res.json(ok(datapacks));
  });

  app.post("/api/v1/datapacks", (req, res) => {
    const name = String(req.body?.name || "").trim();
    if (!name) {
      return res.status(400).json({
        data: null,
        error: { code: "VALIDATION_ERROR", message: "name is required" },
      });
    }

    if (datapacks.some((dp) => dp.name === name)) {
      return res.status(409).json({
        data: null,
        error: { code: "CONFLICT", message: `datapack "${name}" already exists` },
      });
    }

    const id = `dp_${Date.now()}`;
    const createdAt = nowIso();
    const created: Datapack = {
      id,
      name,
      account_id: accountId,
      created_at: createdAt,
      updated_at: createdAt,
    };
    datapacks.unshift(created);
    agentsByDatapack[name] = [
      {
        id: `agt_${Date.now()}`,
        datapack_id: id,
        agent_name: "meko_user_agent",
        agent_metadata: "{}",
        created_at: createdAt,
        updated_at: createdAt,
      },
    ];
    kbByDatapack[name] = req.body?.seed_knowledge_base
      ? [
        {
          id: `kb_${Date.now()}`,
          datapack_id: id,
          knowledge_base_url: String(req.body.seed_knowledge_base),
          created_at: createdAt,
          updated_at: createdAt,
        },
      ]
      : [];

    return res.status(201).json(
      ok({
        datapack_id: id,
        agent_id: agentsByDatapack[name][0].id,
        name,
        mcp_server: {
          url: `https://mekodev.com/${accountId}/mcp`,
          api_key: `meko_dp_${id}_secret_key`,
        },
      }),
    );
  });

  app.get("/api/v1/datapacks/:name/agents", (req, res) => {
    const name = req.params.name;
    res.json(ok(agentsByDatapack[name] || []));
  });

  app.get("/api/v1/datapacks/:name/knowledge-bases", (req, res) => {
    const name = req.params.name;
    res.json(ok(kbByDatapack[name] || []));
  });

  app.get("/api/v1/datapacks/:name/status", (req, res) => {
    const name = req.params.name;
    const dp = datapacks.find((item) => item.name === name);
    if (!dp) {
      return res.status(404).json({
        data: null,
        error: { code: "NOT_FOUND", message: `datapack "${name}" not found` },
      });
    }

    return res.json(
      ok({
        id: dp.id,
        name: dp.name,
        created_at: dp.created_at,
        agent_count: (agentsByDatapack[name] || []).length,
        knowledge_base_count: (kbByDatapack[name] || []).length,
      }),
    );
  });

  return httpServer;
}
