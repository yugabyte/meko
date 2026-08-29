import { ROUTES } from "@/app/routes/paths";

/** ProtectedRoutes doesn't use react-router's declarative <Route> matching —
 *  every page derives its own dynamic segments from the raw pathname (same
 *  convention DatapacksPage uses for :datapack_id). Returns null off /labs. */
export function parseLabRoute(pathname: string): { labId: string; isRunner: boolean } | null {
  if (!pathname.startsWith(ROUTES.labs)) return null;
  const rest = pathname.slice(ROUTES.labs.length);
  const segments = rest.split("/").filter(Boolean);
  if (segments.length === 0) return null;
  const [labId, second] = segments;
  return { labId: decodeURIComponent(labId), isRunner: second === "run" };
}
