export const ROUTES = {
  root: "/",
  login: "/login",
  signin: "/signin",
  signup: "/signup",
  onboarding: "/onboarding",
  home: "/home",
  playground: "/playground",
  datapacks: "/datapacks",
  observeHub: "/observe-hub",
} as const;

/** Default datapack provisioned for new accounts; used from onboarding (“Choose your tool” → edit). */
export const DEFAULT_ACCOUNT_DATAPACK_NAME = "meko_default_datapack" as const;

export function datapackDetailPath(datapackName: string): string {
  return `${ROUTES.datapacks}/${encodeURIComponent(datapackName)}`;
}

export function datapackEditPath(datapackName: string): string {
  return `${ROUTES.datapacks}/${encodeURIComponent(datapackName)}/edit`;
}

export function datapackSharePath(datapackName: string): string {
  return `${ROUTES.datapacks}/${encodeURIComponent(datapackName)}/share`;
}

/** Create-datapack form (reserved segment; not a datapack name). */
export function datapackNewPath(): string {
  return `${ROUTES.datapacks}/new`;
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
] as const;

export const PROTECTED_ROUTE_PREFIXES = [
  ROUTES.onboarding,
  ROUTES.home,
  ROUTES.playground,
  ROUTES.datapacks,
  ROUTES.observeHub,
] as const;
