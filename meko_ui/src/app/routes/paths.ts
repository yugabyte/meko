export const ROUTES = {
  root: "/",
  login: "/login",
  signin: "/signin",
  signup: "/signup",
  onboarding: "/onboarding",
  datapacks: "/datapacks",
  observeHub: "/observe-hub",
} as const;

export const AUTH_ROUTES = [
  ROUTES.root,
  ROUTES.login,
  ROUTES.signin,
  ROUTES.signup,
] as const;

export const PROTECTED_ROUTE_PREFIXES = [
  ROUTES.onboarding,
  ROUTES.datapacks,
  ROUTES.observeHub,
] as const;
