export const ROUTES = {
  root: "/",
  login: "/login",
  signin: "/signin",
  signup: "/signup",
  forgotPassword: "/forgot-password",
  /** First-login Cognito challenge: set password + name (from Admin-created users). */
  completeNewPassword: "/complete-new-password",
  onboarding: "/onboarding",
  home: "/home",
  // URL kept as `/workbench-lm` for backward-compat with existing bookmarks
  // and links; the product is renamed to "WorkbenchLM" elsewhere in the UI.
  workbenchLm: "/workbench-lm",
  datapacks: "/datapacks",
  observeHub: "/observe-hub",
  /** Account-scoped settings page (profile + change password). */
  settings: "/settings",
} as const;

/**
 * Sub-route segments under `/datapacks/:name/...`. Single source of truth
 * shared by the path builders below and the segment matcher in
 * `DatapacksPage.tsx` so a rename can't drift between them.
 */
export const DATAPACK_SUB_ROUTE = {
  new: "new",
  configure: "configure",
  edit: "edit",
  share: "share",
  learnings: "learnings",
  memorySummary: "memory-summary",
  collectiveMemorySummary: "collective-memory-summary",
  knowledge: "knowledge",
  conversations: "conversations",
} as const;

/** Default datapack provisioned for new accounts; used from onboarding (“Choose your tool” → configure). */
export const DEFAULT_ACCOUNT_DATAPACK_NAME = "meko_default_datapack" as const;

export function datapackDetailPath(datapackName: string, datapackId?: string): string {
  return appendDatapackId(
    `${ROUTES.datapacks}/${encodeURIComponent(datapackName)}`,
    datapackId,
  );
}

/**
 * Search param names for deep links into datapack pages (e.g. open a modal from the overview).
 * Use with {@link DATAPACK_QUERY_FLAG_ON} for boolean-style flags.
 */
export const DATAPACK_QUERY_PARAM = {
  connect: "connect",
  share: "share",
} as const;

/** Value meaning “enable / open” for {@link DATAPACK_QUERY_PARAM} flags. */
export const DATAPACK_QUERY_FLAG_ON = "1" as const;

/** Datapack detail URL that opens the Connect dialog (consumes param on load). */
export function datapackDetailUrlOpenConnect(datapackName: string, datapackId?: string): string {
  const q = new URLSearchParams();
  q.set(DATAPACK_QUERY_PARAM.connect, DATAPACK_QUERY_FLAG_ON);
  if (datapackId) q.set(DATAPACK_ID_QUERY_PARAM, datapackId);
  return `${datapackDetailPath(datapackName)}?${q.toString()}`;
}

/**
 * Query param + values used when the conversation viewer is reached via a
 * deep-link from another summary page. The viewer reads this to render an
 * extra breadcrumb hop so the user can step back to where they came from.
 */
export const CONVERSATION_SOURCE_QUERY_PARAM = "from" as const;
export const CONVERSATION_SOURCE = {
  memorySummary: "memory-summary",
  collectiveMemorySummary: "collective-memory-summary",
} as const;
export type ConversationSource =
  (typeof CONVERSATION_SOURCE)[keyof typeof CONVERSATION_SOURCE];

/** Build a conversation viewer URL with a `from` source attached. */
export function datapackConversationsPathWithSource(
  datapackName: string,
  conversationId: string,
  source: ConversationSource,
  extraParams?: Record<string, string>,
  datapackId?: string,
): string {
  // Build the canonical base path (without our own query string), then
  // collect everything into a single URLSearchParams so the resulting
  // URL is well-formed regardless of which optional params are present.
  // We deliberately don't call `datapackConversationsPath(name, id, datapackId)`
  // here because that helper would already attach `?datapack_id=...` and we'd
  // then have to merge two query strings. Cleaner to compose them in one place.
  const base = `${datapackSubPath(datapackName, DATAPACK_SUB_ROUTE.conversations)}/${encodeURIComponent(conversationId)}`;
  const q = new URLSearchParams();
  if (extraParams) {
    for (const [k, v] of Object.entries(extraParams)) q.set(k, v);
  }
  q.set(CONVERSATION_SOURCE_QUERY_PARAM, source);
  if (datapackId) q.set(DATAPACK_ID_QUERY_PARAM, datapackId);
  return `${base}?${q.toString()}`;
}

function datapackSubPath(datapackName: string, segment: string): string {
  return `${ROUTES.datapacks}/${encodeURIComponent(datapackName)}/${segment}`;
}

/**
 * Query-param name used to disambiguate datapacks that share a name across
 * share boundaries (owned vs. accepted-share with the same `datapack_name`).
 * Every datapack sub-route URL appends it when the caller has the id;
 * routes without it fall back to name-only resolution (BE picks first).
 *
 * Kept as a const so the router / handlers / URL builders never drift
 * on the spelling.
 */
export const DATAPACK_ID_QUERY_PARAM = "datapack_id" as const;

/**
 * Append `?datapack_id=<uuid>` to a path. When `datapackId` is empty /
 * undefined the path is returned unchanged so existing bookmarks /
 * deep-links without the id still navigate cleanly.
 *
 * Used by the per-route helpers below — keep all the URL-shape logic
 * in one place so a future rename of the param can't drift between
 * callers.
 */
function appendDatapackId(path: string, datapackId?: string): string {
  if (!datapackId) return path;
  const sep = path.includes("?") ? "&" : "?";
  return `${path}${sep}${DATAPACK_ID_QUERY_PARAM}=${encodeURIComponent(datapackId)}`;
}

export function datapackEditPath(datapackName: string): string {
  return datapackSubPath(datapackName, DATAPACK_SUB_ROUTE.edit);
}

export function datapackSharePath(datapackName: string): string {
  return datapackSubPath(datapackName, DATAPACK_SUB_ROUTE.share);
}

/** Legacy `/datapacks/new` — prefer {@link datapackNewConfigurePath} (wizard includes create step). */
export function datapackNewPath(): string {
  return `${ROUTES.datapacks}/${DATAPACK_SUB_ROUTE.new}`;
}

/** New datapack: opens configure wizard with create flow (`/datapacks/new/configure`). */
export function datapackNewConfigurePath(): string {
  return `${ROUTES.datapacks}/${DATAPACK_SUB_ROUTE.new}/${DATAPACK_SUB_ROUTE.configure}`;
}

export function datapackConfigurePath(datapackName: string): string {
  return datapackSubPath(datapackName, DATAPACK_SUB_ROUTE.configure);
}

export function datapackLearningsPath(datapackName: string, datapackId?: string): string {
  return appendDatapackId(datapackSubPath(datapackName, DATAPACK_SUB_ROUTE.learnings), datapackId);
}

export function datapackMemorySummaryPath(datapackName: string, datapackId?: string): string {
  return appendDatapackId(datapackSubPath(datapackName, DATAPACK_SUB_ROUTE.memorySummary), datapackId);
}

export function datapackCollectiveMemorySummaryPath(
  datapackName: string,
  datapackId?: string,
): string {
  return appendDatapackId(
    datapackSubPath(datapackName, DATAPACK_SUB_ROUTE.collectiveMemorySummary),
    datapackId,
  );
}

export function datapackKnowledgePath(datapackName: string, datapackId?: string): string {
  return appendDatapackId(datapackSubPath(datapackName, DATAPACK_SUB_ROUTE.knowledge), datapackId);
}

/** Full conversation viewer route. When `conversationId` is provided
 *  the viewer pre-selects that session in the left rail; otherwise it
 *  lands on the first row of the newest page. `datapackId` (optional)
 *  pins the URL to a specific datapack so name collisions across
 *  share boundaries don't ambiguously resolve. */
export function datapackConversationsPath(
  datapackName: string,
  conversationId?: string,
  datapackId?: string,
): string {
  const base = datapackSubPath(datapackName, DATAPACK_SUB_ROUTE.conversations);
  const withId = conversationId
    ? `${base}/${encodeURIComponent(conversationId)}`
    : base;
  return appendDatapackId(withId, datapackId);
}

export const AUTH_ROUTES = [
  ROUTES.root,
  ROUTES.login,
  ROUTES.signin,
  ROUTES.signup,
  ROUTES.forgotPassword,
  ROUTES.completeNewPassword,
] as const;

export const PROTECTED_ROUTE_PREFIXES = [
  ROUTES.onboarding,
  ROUTES.home,
  ROUTES.workbenchLm,
  ROUTES.datapacks,
  ROUTES.observeHub,
] as const;
