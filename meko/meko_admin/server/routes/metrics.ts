import { Router } from "express";
import { pool } from "../db";
import { getDailyLogins } from "../cloudwatch";
import { getNeverLoggedInUsers } from "../cognito";

const router = Router();

/** Unwraps a settled result, logging a warning and returning the fallback on rejection. */
function settle<T>(result: PromiseSettledResult<T>, fallback: T, label: string): T {
  if (result.status === "fulfilled") return result.value;
  console.warn(`[metrics] ${label} failed:`, result.reason);
  return fallback;
}

router.get("/", async (_req, res) => {
  const [
    dailyLoginsResult,
    neverLoggedInUsersResult,
    queuedResult,
    datapacksResult,
    documentsResult,
    embeddingsResult,
    totalUsersResult,
    userRegistrationsResult,
  ] = await Promise.allSettled([
    getDailyLogins(7),
    getNeverLoggedInUsers(),
    pool.query<{ count: string }>(
      "SELECT COUNT(*)::text AS count FROM dist_rag.work_queue WHERE task_status = 'QUEUED'"
    ),
    pool.query<{ count: string }>(
      "SELECT COUNT(*)::text AS count FROM meko_system.datapacks"
    ),
    pool.query<{ count: string }>(
      "SELECT COUNT(*)::text AS count FROM dist_rag.documents WHERE status = 'COMPLETED'"
    ),
    pool.query<{ total: string }>(
      "SELECT COALESCE(SUM(embeddings_persisted), 0)::text AS total FROM dist_rag.pipeline_details WHERE status = 'COMPLETED'"
    ),
    pool.query<{ count: string }>(
      "SELECT COUNT(*)::text AS count FROM meko_system.users"
    ),
    pool.query<{ day: string; new_users: string }>(
      `SELECT DATE(created_at AT TIME ZONE 'UTC')::text AS day,
              COUNT(*)::text AS new_users
       FROM meko_system.users
       WHERE created_at >= NOW() - INTERVAL '30 days'
       GROUP BY DATE(created_at AT TIME ZONE 'UTC')
       ORDER BY day`
    ),
  ]);

  const emptyQuery = { rows: [] as never[], rowCount: 0, command: "", oid: 0, fields: [] };

  const dailyLogins       = settle(dailyLoginsResult,        [],         "getDailyLogins");
  const neverLoggedIn     = settle(neverLoggedInUsersResult, [],         "getNeverLoggedInUsers");
  const queued            = settle(queuedResult,             emptyQuery, "queuedWorkItems");
  const datapacks         = settle(datapacksResult,          emptyQuery, "datapacksCreated");
  const documents         = settle(documentsResult,          emptyQuery, "documentsProcessed");
  const embeddings        = settle(embeddingsResult,         emptyQuery, "embeddingsGenerated");
  const totalUsers        = settle(totalUsersResult,         emptyQuery, "totalUsers");
  const userRegistrations = settle(userRegistrationsResult,  emptyQuery, "userRegistrations");

  const registrationsByDay = userRegistrations.rows.map((r) => ({
    date: r.day,
    count: parseInt(r.new_users, 10),
  }));

  res.json({
    usage: {
      dailyLogins,
      neverLoggedInUsers: neverLoggedIn,
      totalUsers: parseInt(totalUsers.rows[0]?.count ?? "0", 10),
      userRegistrations: registrationsByDay,
    },
    system: {
      queuedWorkItems:    parseInt(queued.rows[0]?.count    ?? "0", 10),
      datapacksCreated:   parseInt(datapacks.rows[0]?.count ?? "0", 10),
      documentsProcessed: parseInt(documents.rows[0]?.count ?? "0", 10),
      embeddingsGenerated: parseInt(embeddings.rows[0]?.total ?? "0", 10),
    },
  });
});

export default router;
