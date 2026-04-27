import type { Express } from "express";
import { createServer, type Server } from "http";

type Datapack = {
  datapack_id: string;
  datapack_name: string;
  user_id: string;
  universe_id: string;
  datapack_ysql_connection_string: string;
  datapack_ysql_username: string;
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
    datapack_id: "dp_default_1",
    datapack_name: "default",
    user_id: accountId,
    universe_id: "uni_mock",
    datapack_ysql_connection_string: "postgresql://localhost:5433/yb",
    datapack_ysql_username: "mcp_user",
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

    if (datapacks.some((dp) => dp.datapack_name === name)) {
      return res.status(409).json({
        data: null,
        error: { code: "CONFLICT", message: `datapack "${name}" already exists` },
      });
    }

    const id = `dp_${Date.now()}`;
    const createdAt = nowIso();
    const created: Datapack = {
      datapack_id: id,
      datapack_name: name,
      user_id: accountId,
      universe_id: "uni_mock",
      datapack_ysql_connection_string: "postgresql://localhost:5433/yb",
      datapack_ysql_username: "mcp_user",
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
    const dp = datapacks.find((item) => item.datapack_name === name);
    if (!dp) {
      return res.status(404).json({
        data: null,
        error: { code: "NOT_FOUND", message: `datapack "${name}" not found` },
      });
    }

    return res.json(
      ok({
        datapack_id: dp.datapack_id,
        datapack_name: dp.datapack_name,
        universe_id: dp.universe_id,
        datapack_ysql_connection_string: dp.datapack_ysql_connection_string,
        datapack_ysql_username: dp.datapack_ysql_username,
        created_at: dp.created_at,
        agent_count: (agentsByDatapack[name] || []).length,
        knowledge_base_count: (kbByDatapack[name] || []).length,
        memories_count: 0, // TODO: Once BE supports it, need to update
        learnings_count: 0, // TODO: Once BE supports it, need to update
        conversations_count: 0, // TODO: Once BE supports it, need to update
      }),
    );
  });

  app.patch("/api/v1/datapacks/:name", (req, res) => {
    const name = req.params.name;
    const dp = datapacks.find((item) => item.datapack_name === name);
    if (!dp) {
      return res.status(404).json({
        data: null,
        error: { code: "NOT_FOUND", message: `datapack "${name}" not found` },
      });
    }
    const newName = typeof req.body?.datapack_name === "string" ? req.body.datapack_name.trim() : "";
    const newConn =
      typeof req.body?.datapack_ysql_connection_string === "string"
        ? req.body.datapack_ysql_connection_string.trim()
        : "";
    if (!newName && !newConn) {
      return res.status(400).json({
        data: null,
        error: { code: "VALIDATION_ERROR", message: "no fields to update" },
      });
    }
    if (newName && newName !== name) {
      if (datapacks.some((d) => d.datapack_name === newName)) {
        return res.status(409).json({
          data: null,
          error: { code: "CONFLICT", message: `datapack "${newName}" already exists` },
        });
      }
      dp.datapack_name = newName;
      dp.updated_at = nowIso();
      if (agentsByDatapack[name] !== undefined) {
        agentsByDatapack[newName] = agentsByDatapack[name];
        delete agentsByDatapack[name];
      }
      if (kbByDatapack[name] !== undefined) {
        kbByDatapack[newName] = kbByDatapack[name];
        delete kbByDatapack[name];
      }
    }
    if (newConn) {
      dp.datapack_ysql_connection_string = newConn;
      dp.updated_at = nowIso();
    }
    return res.json(ok({ message: "datapack updated" }));
  });

  app.delete("/api/v1/datapacks/:name", (req, res) => {
    const name = req.params.name;
    const idx = datapacks.findIndex((item) => item.datapack_name === name);
    if (idx === -1) {
      return res.status(404).json({
        data: null,
        error: { code: "NOT_FOUND", message: `datapack "${name}" not found` },
      });
    }
    datapacks.splice(idx, 1);
    delete agentsByDatapack[name];
    delete kbByDatapack[name];
    return res.json(ok({ message: "datapack deleted" }));
  });

  return httpServer;
}
