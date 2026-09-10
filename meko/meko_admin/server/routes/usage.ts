import { Router, type Request } from "express";
import { apiServerGet, apiServerSend } from "../apiServerClient";
import { type AuthenticatedRequest } from "../middleware/requireAdminAuth";

// Forward the shared start/end range params (only when present) to api_server.
function rangeQuery(req: Request): Record<string, string> {
  const query: Record<string, string> = {};
  if (typeof req.query.start === "string") query.start = req.query.start;
  if (typeof req.query.end === "string") query.end = req.query.end;
  return query;
}

// One tier-change audit event (who changed a user's tier, old→new, when).
type TierAuditEntry = {
  actor_email: string;
  actor_uuid?: string;
  source: string;
  old_tier: string;
  new_tier: string;
  created_at: string;
};

// Admin usage metrics, proxied from api_server (which owns the queries across
// the meko_system + data-plane DBs). meko_admin adds no SQL of its own here.
const router = Router();

type AdminUser = {
  id: string;
  email: string;
  first_name: string;
  last_name: string;
  tier: "free" | "pro";
  created_at: string;
};

/** GET /api/admin/usage/users — user list for the dashboard dropdown. */
router.get("/users", async (_req, res, next) => {
  try {
    const data = await apiServerGet<{ users: AdminUser[] }>("/users");
    res.json(data);
  } catch (e) {
    next(e);
  }
});

/** GET /api/admin/usage?user_id=<id|all>&start=&end= — overall or per-user,
 *  optionally windowed by a date range. */
router.get("/", async (req, res, next) => {
  try {
    const query: Record<string, string> = {
      user_id: typeof req.query.user_id === "string" ? req.query.user_id : "all",
    };
    if (typeof req.query.start === "string") query.start = req.query.start;
    if (typeof req.query.end === "string") query.end = req.query.end;
    const data = await apiServerGet<Record<string, unknown>>("/metrics", query);
    res.json(data);
  } catch (e) {
    next(e);
  }
});

/** GET /api/admin/usage/executive/:panel — executive dashboard, split into four
 *  panels (growth, engagement, knowledge, savings) the client fetches in
 *  parallel. Each forwards the start/end range to the matching api_server route. */
for (const panel of ["growth", "engagement", "knowledge", "savings"] as const) {
  router.get(`/executive/${panel}`, async (req, res, next) => {
    try {
      const data = await apiServerGet<Record<string, unknown>>(
        `/executive/${panel}`,
        rangeQuery(req),
      );
      res.json(data);
    } catch (e) {
      next(e);
    }
  });
}

/** GET /api/admin/usage/dataplane — live YBA universe config + utilization. */
router.get("/dataplane", async (_req, res, next) => {
  try {
    const data = await apiServerGet<Record<string, unknown>>("/dataplane");
    res.json(data);
  } catch (e) {
    next(e);
  }
});

/** PATCH /api/admin/usage/users/:id/tier — override a user's tier. Forwards the
 *  authenticated operator so api_server can audit who made the change. */
router.patch("/users/:id/tier", async (req, res, next) => {
  try {
    const { tier } = req.body as { tier?: string };
    const actor = (req as unknown as AuthenticatedRequest).adminUser;
    const data = await apiServerSend<{ id: string; tier: string }>(
      "PATCH",
      `/users/${req.params.id}/tier`,
      { tier },
      { "X-Meko-Admin-Actor-Email": actor.email, "X-Meko-Admin-Actor-Uuid": actor.sub },
    );
    res.json(data);
  } catch (e) {
    next(e);
  }
});

/** GET /api/admin/usage/users/:id/tier-audit — tier-change history for a user. */
router.get("/users/:id/tier-audit", async (req, res, next) => {
  try {
    const data = await apiServerGet<TierAuditEntry[]>(`/users/${req.params.id}/tier-audit`);
    res.json(data);
  } catch (e) {
    next(e);
  }
});

export default router;
