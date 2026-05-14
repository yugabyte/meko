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
  // URL kept as `/playground` for backward-compat with existing bookmarks
  // and links; the product is renamed to "WorkbenchLM" elsewhere in the UI.
  workbenchLm: "/playground",
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

export function datapackDetailPath(datapackName: string): string {
  return `${ROUTES.datapacks}/${encodeURIComponent(datapackName)}`;
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
export function datapackDetailUrlOpenConnect(datapackName: string): string {
  const q = new URLSearchParams();
  q.set(DATAPACK_QUERY_PARAM.connect, DATAPACK_QUERY_FLAG_ON);
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
): string {
  const base = datapackConversationsPath(datapackName, conversationId);
  const q = new URLSearchParams();
  if (extraParams) {
    for (const [k, v] of Object.entries(extraParams)) q.set(k, v);
  }
  q.set(CONVERSATION_SOURCE_QUERY_PARAM, source);
  return `${base}?${q.toString()}`;
}

function datapackSubPath(datapackName: string, segment: string): string {
  return `${ROUTES.datapacks}/${encodeURIComponent(datapackName)}/${segment}`;
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

export function datapackLearningsPath(datapackName: string): string {
  return datapackSubPath(datapackName, DATAPACK_SUB_ROUTE.learnings);
}

export function datapackMemorySummaryPath(datapackName: string): string {
  return datapackSubPath(datapackName, DATAPACK_SUB_ROUTE.memorySummary);
}

export function datapackCollectiveMemorySummaryPath(datapackName: string): string {
  return datapackSubPath(datapackName, DATAPACK_SUB_ROUTE.collectiveMemorySummary);
}

export function datapackKnowledgePath(datapackName: string): string {
  return datapackSubPath(datapackName, DATAPACK_SUB_ROUTE.knowledge);
}

/** Full conversation viewer route. When `conversationId` is provided
 *  the viewer pre-selects that session in the left rail; otherwise it
 *  lands on the first row of the newest page. */
export function datapackConversationsPath(
  datapackName: string,
  conversationId?: string,
): string {
  const base = datapackSubPath(datapackName, DATAPACK_SUB_ROUTE.conversations);
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
  ROUTES.home,
  ROUTES.workbenchLm,
  ROUTES.datapacks,
  ROUTES.observeHub,
] as const;
