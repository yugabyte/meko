import { useLocation } from "react-router-dom";

import NotFoundPage from "@/features/system/pages/NotFoundPage";

import { parseLabRoute } from "../lib/parseLabRoute";
import { useLabsNavFlag } from "../hooks/useLabsNavFlag";
import LabDetailPage from "./LabDetailPage";
import LabsListPage from "./LabsListPage";

/** ProtectedRoutes mounts this for any /labs/* path that isn't the runner
 *  (that's mounted/unmounted separately since it polls) — switches between
 *  the list and detail page by parsing the pathname, same convention
 *  DatapacksPage uses since this app doesn't use declarative <Route>
 *  matching for the protected area.
 *
 *  Pre-GA: the flag gates more than the Sidebar entry -- self-guard here too
 *  so a direct URL hit bounces, same as BillingPage/AdminPage. */
export default function LabsSection() {
  const location = useLocation();
  const showLabs = useLabsNavFlag();
  if (!showLabs) return <NotFoundPage />;
  const parsed = parseLabRoute(location.pathname);
  return parsed ? <LabDetailPage /> : <LabsListPage />;
}
