export const ROUTES = {
  root: "/",
  login: "/login",
  signin: "/signin",
  signup: "/signup",
  forgotPassword: "/forgot-password",
  /** First-login Cognito challenge: set password + name (from Admin-created users). */
  completeNewPassword: "/complete-new-password",
  onboarding: "/onboarding",
  /** Connect (MCP install) page. URL changed from `/home` → `/connect` when Connect was promoted out of the Welcome nest into a top-level sidebar item. */
  connect: "/connect",
  // URL kept as `/workbench-lm` for backward-compat with existing bookmarks
  // and links; the product is renamed to "WorkbenchLM" elsewhere in the UI.
  workbenchLm: "/workbench-lm",
  datapacks: "/datapacks",
  observeHub: "/observe-hub",
  /** Account-scoped settings page (profile + change password). */
  settings: "/settings",
  /** Workspace plans & billing (current plan, usage, plan picker). */
  billing: "/billing",
} as const;

/**
 * Sub-route segments under `/datapacks/:datapack_id/...`. Single source of truth
 * shared by the path builders below and the segment matcher in
 * `DatapacksPage.tsx`.
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
  members: "members",
  conversations: "conversations",
  databases: "databases",
} as const;

/** Default datapack provisioned for new accounts; used from onboarding (“Choose your tool” → configure). */
export const DEFAULT_ACCOUNT_DATAPACK_NAME = "meko_default_datapack" as const;

export function datapackDetailPath(datapackId: string): string {
  return `${ROUTES.datapacks}/${encodeURIComponent(datapackId)}`;
}

/**
 * Search param names for deep links into datapack pages (e.g. open a modal from the overview).
 * Use with {@link DATAPACK_QUERY_FLAG_ON} for boolean-style flags.
 */
export const DATAPACK_QUERY_PARAM = {
  connect: "connect",
  share: "share",
} as const;

/** Query param used by non-`/datapacks/:id` surfaces such as WorkbenchLM. */
export const DATAPACK_ID_QUERY_PARAM = "datapack_id" as const;

/** Value meaning “enable / open” for {@link DATAPACK_QUERY_PARAM} flags. */
export const DATAPACK_QUERY_FLAG_ON = "1" as const;

/** Datapack detail URL that opens the Connect dialog (consumes param on load). */
export function datapackDetailUrlOpenConnect(datapackId: string): string {
  const q = new URLSearchParams();
  q.set(DATAPACK_QUERY_PARAM.connect, DATAPACK_QUERY_FLAG_ON);
  return `${datapackDetailPath(datapackId)}?${q.toString()}`;
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
  datapackId: string,
  conversationId: string,
  source: ConversationSource,
  extraParams?: Record<string, string>,
): string {
  const base = `${datapackSubPath(datapackId, DATAPACK_SUB_ROUTE.conversations)}/${encodeURIComponent(conversationId)}`;
  const q = new URLSearchParams();
  if (extraParams) {
    for (const [k, v] of Object.entries(extraParams)) q.set(k, v);
  }
  q.set(CONVERSATION_SOURCE_QUERY_PARAM, source);
  return `${base}?${q.toString()}`;
}

function datapackSubPath(datapackId: string, segment: string): string {
  return `${datapackDetailPath(datapackId)}/${segment}`;
}

export function datapackEditPath(datapackId: string): string {
  return datapackSubPath(datapackId, DATAPACK_SUB_ROUTE.edit);
}

export function datapackSharePath(datapackId: string): string {
  return datapackSubPath(datapackId, DATAPACK_SUB_ROUTE.share);
}

/** Legacy `/datapacks/new` — prefer {@link datapackNewConfigurePath} (wizard includes create step). */
export function datapackNewPath(): string {
  return `${ROUTES.datapacks}/${DATAPACK_SUB_ROUTE.new}`;
}

/** New datapack: opens configure wizard with create flow (`/datapacks/new/configure`). */
export function datapackNewConfigurePath(): string {
  return `${ROUTES.datapacks}/${DATAPACK_SUB_ROUTE.new}/${DATAPACK_SUB_ROUTE.configure}`;
}

export function datapackConfigurePath(datapackId: string): string {
  return datapackSubPath(datapackId, DATAPACK_SUB_ROUTE.configure);
}

export function datapackLearningsPath(datapackId: string): string {
  return datapackSubPath(datapackId, DATAPACK_SUB_ROUTE.learnings);
}

/**
 * Sources for the `?from=` query param on the Learnings page. When set,
 * the Learnings breadcrumb trail grows an extra hop back to the page
 * the user actually came from (e.g. SharedKnowledge → Promoted
 * memories tab fired "Review N pending").
 *
 * Reuses the same pattern as CONVERSATION_SOURCE so all "I came from X"
 * routing flows through a single closed enum.
 */
export const LEARNINGS_SOURCE_QUERY_PARAM = "from" as const;
export const LEARNINGS_SOURCE = {
  sharedKnowledge: "shared-knowledge",
} as const;
export type LearningsSource =
  (typeof LEARNINGS_SOURCE)[keyof typeof LEARNINGS_SOURCE];

/** Build a Learnings URL with a `from` source attached (used by
 *  "Review N pending" on the SharedKnowledge "Promoted memories" tab
 *  to deep-link into review while preserving the breadcrumb trail). */
export function datapackLearningsPathFrom(
  datapackId: string,
  source: LearningsSource,
): string {
  const base = datapackLearningsPath(datapackId);
  const q = new URLSearchParams();
  q.set(LEARNINGS_SOURCE_QUERY_PARAM, source);
  return `${base}?${q.toString()}`;
}

export function datapackMemorySummaryPath(datapackId: string): string {
  return datapackSubPath(datapackId, DATAPACK_SUB_ROUTE.memorySummary);
}

export function datapackCollectiveMemorySummaryPath(
  datapackId: string,
): string {
  return datapackSubPath(datapackId, DATAPACK_SUB_ROUTE.collectiveMemorySummary);
}

export function datapackKnowledgePath(datapackId: string): string {
  return datapackSubPath(datapackId, DATAPACK_SUB_ROUTE.knowledge);
}

/** Full members roster page — linked from the "View all" footer on
 *  the Members tile on the datapack detail page. */
export function datapackMembersPath(datapackId: string): string {
  return datapackSubPath(datapackId, DATAPACK_SUB_ROUTE.members);
}

/** Full databases page (AMP) — linked from the "View all" footer on the
 *  Databases tile. Gated by SHOW_PG_AMP at the call sites. */
export function datapackDatabasesPath(datapackId: string): string {
  return datapackSubPath(datapackId, DATAPACK_SUB_ROUTE.databases);
}

/** Full conversation viewer route. When `conversationId` is provided
 *  the viewer pre-selects that session in the left rail; otherwise it
 *  lands on the first row of the newest page. */
export function datapackConversationsPath(
  datapackId: string,
  conversationId?: string,
): string {
  const base = datapackSubPath(datapackId, DATAPACK_SUB_ROUTE.conversations);
  return conversationId
    ? `${base}/${encodeURIComponent(conversationId)}`
    : base;
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
  ROUTES.connect,
  ROUTES.workbenchLm,
  ROUTES.datapacks,
  ROUTES.observeHub,
  ROUTES.settings,
] as const;
