import { Router } from "express";
import { apiServerGet } from "../apiServerClient";

// Admin feedback list, proxied from api_server (which reads
// meko_system.feedback under the admin secret). meko_admin adds no
// DB work of its own here — mirrors the shape used by usage.ts.
const router = Router();

// Kept as a permissive Record so we don't have to re-encode the whole
// AdminFeedbackItem shape here — the FE has the canonical type in
// src/lib/adminApi.ts. Node just forwards the JSON verbatim.
type AdminFeedbackListResponse = {
  feedback: Array<Record<string, unknown>>;
  pagination: { total: number; limit: number; offset: number };
};

/** GET /api/admin/feedback?from=&to=&limit=&offset= — paged feedback filtered
 *  by created_at range, newest first. All params optional; api_server owns
 *  the parsing / clamping. */
router.get("/", async (req, res, next) => {
  try {
    const query: Record<string, string> = {};
    for (const key of ["from", "to", "limit", "offset"] as const) {
      const value = req.query[key];
      if (typeof value === "string" && value.trim() !== "") {
        query[key] = value.trim();
      }
    }
    const data = await apiServerGet<AdminFeedbackListResponse>("/feedback", query);
    res.json(data);
  } catch (e) {
    next(e);
  }
});

export default router;
