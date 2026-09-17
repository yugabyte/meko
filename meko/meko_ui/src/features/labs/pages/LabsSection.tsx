import { useLocation } from "react-router-dom";

import { parseLabRoute } from "../lib/parseLabRoute";
import LabDetailPage from "./LabDetailPage";
import LabsListPage from "./LabsListPage";

/** ProtectedRoutes mounts this for any /labs/* path that isn't the runner
 *  (that's mounted/unmounted separately since it polls) — switches between
 *  the list and detail page by parsing the pathname, same convention
 *  DatapacksPage uses since this app doesn't use declarative <Route>
 *  matching for the protected area. */
export default function LabsSection() {
  const location = useLocation();
  const parsed = parseLabRoute(location.pathname);
  return parsed ? <LabDetailPage /> : <LabsListPage />;
}
