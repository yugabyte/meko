// Thin fetch wrapper for the Meko Controller REST API.
//
// Base URL resolves from `VITE_API_BASE_URL`; in dev it defaults to the
// relative `/api/v1`, which Vite proxies to the controller on :8080 (see
// vite.config.ts) so there's no CORS to deal with. In a deployed build set
// `VITE_API_BASE_URL` to the controller's absolute origin + `/api/v1`.

const API_BASE = import.meta.env.VITE_API_BASE_URL ?? "/api/v1";

/** Error carrying the HTTP status so callers/UI can branch on it. */
export class ApiError extends Error {
  constructor(
    message: string,
    readonly status: number,
  ) {
    super(message);
    this.name = "ApiError";
  }
}

/** Pull a human message out of the controller's error body, tolerating its
 *  nested `{error:{message}}` shape, echo's flat `{message}`, a bare
 *  `{error:"…"}` string, or non-JSON text. Returns null if nothing usable. */
function extractErrorMessage(text: string): string | null {
  try {
    const parsed = JSON.parse(text) as {
      message?: unknown;
      error?: unknown;
    };
    const err = parsed.error;
    if (err && typeof err === "object" && "message" in err) {
      const m = (err as { message?: unknown }).message;
      if (typeof m === "string") return m;
    }
    if (typeof parsed.message === "string") return parsed.message;
    if (typeof err === "string") return err;
    return null;
  } catch {
    return null;
  }
}

type RequestOptions = {
  method?: string;
  body?: unknown;
  /** Query params; undefined/null values are dropped. */
  query?: Record<string, string | number | boolean | undefined | null>;
};

function buildUrl(path: string, query?: RequestOptions["query"]): string {
  // Normalize so a trailing slash on VITE_API_BASE_URL (or a path missing its
  // leading slash) can't produce a double slash like /api/v1//users.
  const base = API_BASE.endsWith("/") ? API_BASE.slice(0, -1) : API_BASE;
  const cleanPath = path.startsWith("/") ? path : `/${path}`;
  const url = `${base}${cleanPath}`;
  if (!query) return url;
  const params = new URLSearchParams();
  for (const [key, value] of Object.entries(query)) {
    if (value !== undefined && value !== null) params.set(key, String(value));
  }
  const qs = params.toString();
  return qs ? `${url}?${qs}` : url;
}

export async function apiRequest<T>(
  path: string,
  { method = "GET", body, query }: RequestOptions = {},
): Promise<T> {
  const res = await fetch(buildUrl(path, query), {
    method,
    headers: body ? { "Content-Type": "application/json" } : undefined,
    body: body ? JSON.stringify(body) : undefined,
  });

  if (!res.ok) {
    // Best-effort: surface the server's error message. The controller wraps
    // errors as {"error":{"code","message"}}; echo's own errors use {"message"};
    // either may also be a bare string or plain text.
    let detail = res.statusText;
    try {
      const text = await res.text();
      if (text) {
        detail = extractErrorMessage(text) ?? text;
      }
    } catch {
      // ignore — fall back to statusText
    }
    throw new ApiError(detail || `Request failed (${res.status})`, res.status);
  }

  // 204 / empty body
  if (res.status === 204) return undefined as T;
  const text = await res.text();
  return (text ? JSON.parse(text) : undefined) as T;
}

/** A high limit the controller's list endpoints accept; we page-by-1000 to
 *  fetch "everything" without a paginator (matches the controller UI). */
export const LIST_LIMIT = 1000;
