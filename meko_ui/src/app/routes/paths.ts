export const ROUTES = {
  root: "/",
  login: "/login",
  signin: "/signin",
  signup: "/signup",
  onboarding: "/onboarding",
  home: "/home",
  datapacks: "/datapacks",
  observeHub: "/observe-hub",
} as const;

export function datapackDetailPath(datapackName: string): string {
  return `${ROUTES.datapacks}/${encodeURIComponent(datapackName)}`;
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
  ROUTES.datapacks,
  ROUTES.observeHub,
] as const;
