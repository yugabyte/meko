// Thin client for api_server's admin endpoints. meko_admin proxies business
// metrics here (api_server owns the multi-DB + Langfuse queries) instead of
// running its own SQL. Auth is a shared secret sent as X-Meko-Admin-Secret.

const BASE_URL = process.env.MEKO_API_URL ?? "";
const ADMIN_SECRET = process.env.ADMIN_API_KEY ?? "";

/** GET an api_server admin route and return the unwrapped `data` payload. */
export async function apiServerGet<T>(path: string, query?: Record<string, string>): Promise<T> {
  if (!BASE_URL) throw new Error("MEKO_API_URL is not configured");
  const url = new URL(`/api/v1/admin${path}`, BASE_URL);
  for (const [k, v] of Object.entries(query ?? {})) url.searchParams.set(k, v);

  const resp = await fetch(url, { headers: { "X-Meko-Admin-Secret": ADMIN_SECRET } });
  const body = (await resp.json().catch(() => ({}))) as { data?: T; error?: { message?: string } };
  if (!resp.ok) {
    throw new Error(body.error?.message ?? `api_server ${resp.status} for ${path}`);
  }
  return body.data as T;
}

/** Send a method+JSON body to an api_server admin route; returns `data`. */
export async function apiServerSend<T>(method: string, path: string, body?: unknown): Promise<T> {
  if (!BASE_URL) throw new Error("MEKO_API_URL is not configured");
  const url = new URL(`/api/v1/admin${path}`, BASE_URL);

  const resp = await fetch(url, {
    method,
    headers: { "X-Meko-Admin-Secret": ADMIN_SECRET, "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const payload = (await resp.json().catch(() => ({}))) as { data?: T; error?: { message?: string } };
  if (!resp.ok) {
    throw new Error(payload.error?.message ?? `api_server ${resp.status} for ${path}`);
  }
  return payload.data as T;
}
