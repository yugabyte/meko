import type { Express } from "express";
import { createServer, type Server } from "http";
import {
  type DatapackShareGrant,
  DATAPACK_SHARE_GRANT_MAINTAINER,
  DATAPACK_SHARE_GRANT_VALIDATION_MESSAGE,
  isDatapackShareGrant,
} from "../src/lib/datapackShareGrants";

type Datapack = {
  datapack_id: string;
  datapack_name: string;
  user_id: string;
  universe_id: string;
  datapack_ysql_connection_string: string;
  datapack_ysql_username: string;
  created_at: string;
  updated_at: string;
  description?: string;
};

type Agent = {
  agent_id: string;
  user_id: string;
  agent_details?: string;
  created_at: string;
  updated_at: string;
};

type KnowledgeBase = {
  rag_source_id: string;
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
    description: "",
  },
];

const agentsByDatapack: Record<string, Agent[]> = {
  default: [
    {
      agent_id: "agt_default_1",
      user_id: accountId,
      agent_details: JSON.stringify({ name: "meko_user_agent" }),
      created_at: nowIso(),
      updated_at: nowIso(),
    },
  ],
};

const kbByDatapack: Record<string, KnowledgeBase[]> = {
  default: [],
};

type MockOutgoingMember = {
  user_id: string;
  is_owner: boolean;
  user_name: string;
  user_email_id: string;
  grant: DatapackShareGrant;
};

type MockPendingInvite = {
  invite_id: string;
  grant: DatapackShareGrant;
  recipient_user_id: string;
  recipient_user_name: string;
  recipient_email_id: string;
};

type MockShareState = {
  members: MockOutgoingMember[];
  pending: MockPendingInvite[];
};

/** In-memory share/members state for local dev when requests hit this mock (no upstream proxy). */
const shareStateByDatapack: Record<string, MockShareState> = {};

const MOCK_OWNER_EMAIL = "dev@meko.local";

function getShareState(datapackName: string): MockShareState | null {
  const dp = datapacks.find((d) => d.datapack_name === datapackName);
  if (!dp) return null;
  if (!shareStateByDatapack[datapackName]) {
    shareStateByDatapack[datapackName] = {
      members: [
        {
          user_id: dp.user_id,
          is_owner: true,
          user_name: "Dev user",
          user_email_id: MOCK_OWNER_EMAIL,
          grant: DATAPACK_SHARE_GRANT_MAINTAINER,
        },
      ],
      pending: [],
    };
  }
  return shareStateByDatapack[datapackName];
}

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

  app.get("/api/v1/datapacks/:name", (req, res) => {
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
        ...dp,
        description: dp.description ?? "",
        memory_count: 0,
        knowledge_count: (kbByDatapack[name] || []).length,
        learnings_count: 0,
        conversations_count: 0,
      }),
    );
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
    const desc =
      typeof req.body?.description === "string" ? String(req.body.description).trim() : "";
    const created: Datapack = {
      datapack_id: id,
      datapack_name: name,
      user_id: accountId,
      universe_id: "uni_mock",
      datapack_ysql_connection_string: "postgresql://localhost:5433/yb",
      datapack_ysql_username: "mcp_user",
      created_at: createdAt,
      updated_at: createdAt,
      description: desc,
    };
    datapacks.unshift(created);
    const agentId = `agt_${Date.now()}`;
    agentsByDatapack[name] = [
      {
        agent_id: agentId,
        user_id: accountId,
        agent_details: JSON.stringify({ name: "meko_user_agent" }),
        created_at: createdAt,
        updated_at: createdAt,
      },
    ];
    kbByDatapack[name] = req.body?.seed_knowledge_base
      ? [
        {
          rag_source_id: `kb_${Date.now()}`,
          datapack_id: id,
          knowledge_base_url: String(req.body.seed_knowledge_base),
          created_at: createdAt,
          updated_at: createdAt,
        },
      ]
      : [];

    shareStateByDatapack[name] = {
      members: [
        {
          user_id: accountId,
          is_owner: true,
          user_name: "Dev user",
          user_email_id: MOCK_OWNER_EMAIL,
          grant: DATAPACK_SHARE_GRANT_MAINTAINER,
        },
      ],
      pending: [],
    };

    return res.status(201).json(
      ok({
        datapack_id: id,
        agent_id: agentsByDatapack[name][0].agent_id,
        name,
        mcp_user_name: "mcp_user",
        mcp_server: {
          url: `https://mekodev.com/${accountId}/mcp`,
          api_key: `meko_dp_${id}_secret_key`,
        },
        db_access: {
          user_name: "mcp_user",
          password: "mock",
          conn_str: "postgresql://localhost:5433/yb",
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

  // --- Datapack sharing (mirrors mekoApi share helpers for local mock when MEKO_API_UPSTREAM is unset) ---
  app.get("/api/v1/datapacks/:name/outgoing-shares", (req, res) => {
    const name = req.params.name;
    const st = getShareState(name);
    if (!st) {
      return res.status(404).json({
        data: null,
        error: { code: "NOT_FOUND", message: `datapack "${name}" not found` },
      });
    }
    return res.json(ok({ pending_invites: st.pending }));
  });

  app.get("/api/v1/datapacks/:name/member-list", (req, res) => {
    const name = req.params.name;
    const st = getShareState(name);
    if (!st) {
      return res.status(404).json({
        data: null,
        error: { code: "NOT_FOUND", message: `datapack "${name}" not found` },
      });
    }
    return res.json(ok({ members: st.members }));
  });

  app.post("/api/v1/datapacks/:name/share", (req, res) => {
    const name = req.params.name;
    const st = getShareState(name);
    if (!st) {
      return res.status(404).json({
        data: null,
        error: { code: "NOT_FOUND", message: `datapack "${name}" not found` },
      });
    }
    const recipient_email = String(req.body?.recipient_email ?? "").trim();
    const grant = req.body?.grant;
    if (!recipient_email) {
      return res.status(400).json({
        data: null,
        error: { code: "VALIDATION_ERROR", message: "recipient_email is required" },
      });
    }
    if (!isDatapackShareGrant(grant)) {
      return res.status(400).json({
        data: null,
        error: { code: "VALIDATION_ERROR", message: DATAPACK_SHARE_GRANT_VALIDATION_MESSAGE },
      });
    }
    const invite_id = `inv_${Date.now()}`;
    const local = recipient_email.split("@")[0] || "invitee";
    st.pending.push({
      invite_id,
      grant,
      recipient_user_id: "",
      recipient_user_name: local,
      recipient_email_id: recipient_email,
    });
    return res.status(201).json(
      ok({
        status: "sent",
        invite_id,
        recipient_email,
        grant,
      }),
    );
  });

  app.post("/api/v1/datapacks/:name/invite/:inviteId/resend", (req, res) => {
    const name = req.params.name;
    const inviteId = req.params.inviteId;
    const st = getShareState(name);
    if (!st) {
      return res.status(404).json({
        data: null,
        error: { code: "NOT_FOUND", message: `datapack "${name}" not found` },
      });
    }
    if (!st.pending.some((p) => p.invite_id === inviteId)) {
      return res.status(404).json({
        data: null,
        error: { code: "NOT_FOUND", message: "invite not found" },
      });
    }
    return res.json(ok({}));
  });

  app.delete("/api/v1/datapacks/:name/invite/:inviteId", (req, res) => {
    const name = req.params.name;
    const inviteId = req.params.inviteId;
    const st = getShareState(name);
    if (!st) {
      return res.status(404).json({
        data: null,
        error: { code: "NOT_FOUND", message: `datapack "${name}" not found` },
      });
    }
    const idx = st.pending.findIndex((p) => p.invite_id === inviteId);
    if (idx === -1) {
      return res.status(404).json({
        data: null,
        error: { code: "NOT_FOUND", message: "invite not found" },
      });
    }
    st.pending.splice(idx, 1);
    return res.json(ok({}));
  });

  app.patch("/api/v1/datapacks/:name/share/:shareUserId", (req, res) => {
    const name = req.params.name;
    const shareUserId = req.params.shareUserId;
    const st = getShareState(name);
    if (!st) {
      return res.status(404).json({
        data: null,
        error: { code: "NOT_FOUND", message: `datapack "${name}" not found` },
      });
    }
    const grant = (req.body as { grant?: unknown } | undefined)?.grant;
    if (!isDatapackShareGrant(grant)) {
      return res.status(400).json({
        data: null,
        error: { code: "VALIDATION_ERROR", message: DATAPACK_SHARE_GRANT_VALIDATION_MESSAGE },
      });
    }
    const member = st.members.find((m) => m.user_id === shareUserId);
    if (!member) {
      return res.status(404).json({
        data: null,
        error: { code: "NOT_FOUND", message: "member not found" },
      });
    }
    if (member.is_owner) {
      return res.status(400).json({
        data: null,
        error: { code: "VALIDATION_ERROR", message: "cannot change owner grant" },
      });
    }
    member.grant = grant;
    return res.json(ok({}));
  });

  app.delete("/api/v1/datapacks/:name/share/:shareUserId", (req, res) => {
    const name = req.params.name;
    const shareUserId = req.params.shareUserId;
    const st = getShareState(name);
    if (!st) {
      return res.status(404).json({
        data: null,
        error: { code: "NOT_FOUND", message: `datapack "${name}" not found` },
      });
    }
    const idx = st.members.findIndex((m) => m.user_id === shareUserId);
    if (idx === -1) {
      return res.status(404).json({
        data: null,
        error: { code: "NOT_FOUND", message: "member not found" },
      });
    }
    if (st.members[idx].is_owner) {
      return res.status(400).json({
        data: null,
        error: { code: "VALIDATION_ERROR", message: "cannot remove owner" },
      });
    }
    st.members.splice(idx, 1);
    return res.json(ok({}));
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
    const body = req.body as Record<string, unknown> | undefined;
    const fromName = typeof body?.name === "string" ? body.name.trim() : "";
    const legacyDatapackName =
      typeof body?.datapack_name === "string" ? String(body.datapack_name).trim() : "";
    const newName = fromName || legacyDatapackName;
    const newConn =
      typeof body?.datapack_ysql_connection_string === "string"
        ? String(body.datapack_ysql_connection_string).trim()
        : "";
    const newDesc = typeof body?.description === "string" ? String(body.description).trim() : "";
    if (!newName && !newConn && !newDesc) {
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
      if (shareStateByDatapack[name] !== undefined) {
        shareStateByDatapack[newName] = shareStateByDatapack[name];
        delete shareStateByDatapack[name];
      }
    }
    if (newConn) {
      dp.datapack_ysql_connection_string = newConn;
      dp.updated_at = nowIso();
    }
    if (newDesc) {
      dp.description = newDesc;
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
    delete shareStateByDatapack[name];
    return res.json(ok({ message: "datapack deleted" }));
  });

  return httpServer;
}
