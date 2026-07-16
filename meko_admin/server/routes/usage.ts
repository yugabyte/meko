import { Router } from "express";
import { apiServerGet, apiServerSend } from "../apiServerClient";

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

/** GET /api/admin/usage/token-savings — executive dashboard summary
 *  (headcount totals + account-wide token savings + top-owner leaderboard). */
router.get("/token-savings", async (_req, res, next) => {
  try {
    const data = await apiServerGet<Record<string, unknown>>("/token-savings");
    res.json(data);
  } catch (e) {
    next(e);
  }
});

/** GET /api/admin/usage/dataplane — live YBA universe config + utilization. */
router.get("/dataplane", async (_req, res, next) => {
  try {
    const data = await apiServerGet<Record<string, unknown>>("/dataplane");
    res.json(data);
  } catch (e) {
    next(e);
  }
});

/** PATCH /api/admin/usage/users/:id/tier — override a user's tier. */
router.patch("/users/:id/tier", async (req, res, next) => {
  try {
    const { tier } = req.body as { tier?: string };
    const data = await apiServerSend<{ id: string; tier: string }>(
      "PATCH",
      `/users/${req.params.id}/tier`,
      { tier },
    );
    res.json(data);
  } catch (e) {
    next(e);
  }
});

export default router;
