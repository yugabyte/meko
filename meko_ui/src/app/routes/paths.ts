export const ROUTES = {
  root: "/",
  login: "/login",
  signin: "/signin",
  signup: "/signup",
  forgotPassword: "/forgot-password",
  onboarding: "/onboarding",
  home: "/home",
  playground: "/playground",
  datapacks: "/datapacks",
  observeHub: "/observe-hub",
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

export function datapackEditPath(datapackName: string): string {
  return `${ROUTES.datapacks}/${encodeURIComponent(datapackName)}/edit`;
}

export function datapackSharePath(datapackName: string): string {
  return `${ROUTES.datapacks}/${encodeURIComponent(datapackName)}/share`;
}

/** Legacy `/datapacks/new` — prefer {@link datapackNewConfigurePath} (wizard includes create step). */
export function datapackNewPath(): string {
  return `${ROUTES.datapacks}/new`;
}

/** New datapack: opens configure wizard with create flow (`/datapacks/new/configure`). */
export function datapackNewConfigurePath(): string {
  return `${ROUTES.datapacks}/new/configure`;
}

export function datapackConfigurePath(datapackName: string): string {
  return `${ROUTES.datapacks}/${encodeURIComponent(datapackName)}/configure`;
}

export function datapackLearningsPath(datapackName: string): string {
  return `${ROUTES.datapacks}/${encodeURIComponent(datapackName)}/learnings`;
}

export function datapackMemorySummaryPath(datapackName: string): string {
  return `${ROUTES.datapacks}/${encodeURIComponent(datapackName)}/memory-summary`;
}

export const AUTH_ROUTES = [
  ROUTES.root,
  ROUTES.login,
  ROUTES.signin,
  ROUTES.signup,
  ROUTES.forgotPassword,
] as const;

export const PROTECTED_ROUTE_PREFIXES = [
  ROUTES.onboarding,
  ROUTES.home,
  ROUTES.playground,
  ROUTES.datapacks,
  ROUTES.observeHub,
] as const;
