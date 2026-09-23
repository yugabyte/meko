// Routing shell for the Collections area: the list at /collections and one collection
// at /collections/:collectionId. Mirrors DatapacksPage's segment matcher rather than
// nesting react-router routes, so the two areas stay consistent.

import { Navigate, useLocation } from "react-router-dom";
import { ROUTES } from "@/app/routes/paths";
import CollectionsOverviewPage from "@/features/collections/pages/CollectionsOverviewPage";
import CollectionDetailPage from "@/features/collections/pages/CollectionDetailPage";

// Same lenient UUID shape DatapacksPage accepts: a route guard, not a strict
// RFC check, so newer schemes (UUIDv7) still route.
const UUID_PATH_SEGMENT_RE: RegExp =
  /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

export default function CollectionsSection() {
  const location = useLocation();
  const prefix = `${ROUTES.collections}/`;

  if (location.pathname.startsWith(prefix)) {
    const segments = location.pathname.slice(prefix.length).split("/").filter(Boolean);
    const collectionId = segments[0] ? decodeURIComponent(segments[0]) : "";

    if (!UUID_PATH_SEGMENT_RE.test(collectionId) || segments.length > 1) {
      return <Navigate to={ROUTES.collections} replace />;
    }
    return <CollectionDetailPage collectionId={collectionId} />;
  }

  return <CollectionsOverviewPage />;
}
