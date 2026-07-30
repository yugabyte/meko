import { Navigate, useLocation } from "react-router-dom";
import { DATAPACK_SUB_ROUTE, datapackNewConfigurePath, ROUTES } from "@/app/routes/paths";
import DatapackConfigureWizardPage from "@/features/datapacks/pages/DatapackConfigureWizardPage";
import DatapackConversationsViewerPage from "@/features/datapacks/pages/DatapackConversationsViewerPage";
import DatapackDetailPage from "@/features/datapacks/pages/DatapackDetailPage";
import DatapackEditPage from "@/features/datapacks/pages/DatapackEditPage";
import DatapackLearningsPage from "@/features/datapacks/pages/DatapackLearningsPage";
import DatapackMemorySummaryPage from "@/features/datapacks/pages/DatapackMemorySummaryPage";
import DatapackCollectiveMemorySummaryPage from "@/features/datapacks/pages/DatapackCollectiveMemorySummaryPage";
import DatapackKnowledgeSummaryPage from "@/features/datapacks/pages/DatapackKnowledgeSummaryPage";
import DatapackMembersListPage from "@/features/datapacks/pages/DatapackMembersListPage";
import DatapackClustersPage from "@/features/datapacks/pages/DatapackClustersPage";
import DatapackSharePage from "@/features/datapacks/pages/DatapackSharePage";
import DatapacksOverviewPage from "@/features/datapacks/pages/DatapacksOverviewPage";
import { useCurrentUser } from "@/hooks/useCurrentUser";

// Matches any 36-char canonical UUID without pinning version/variant nibbles,
// so newer schemes (e.g. UUIDv7) are accepted too. Mirrors the backend's
// lenient uuid.Parse — this is a route-shape guard, not a strict RFC check.
const UUID_PATH_SEGMENT_RE: RegExp = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

/**
 * Datapacks area: overview at `/datapacks`, new datapack wizard at `/datapacks/new/configure`, edit at `/datapacks/:datapack_id/edit`,
 * configure at `/datapacks/:datapack_id/configure`, learnings at `/datapacks/:datapack_id/learnings`,
 * memory-summary at `/datapacks/:datapack_id/memory-summary`,
 * collective-memory-summary at `/datapacks/:datapack_id/collective-memory-summary`,
 * knowledge at `/datapacks/:datapack_id/knowledge`,
 * members at `/datapacks/:datapack_id/members`,
 * conversations viewer at `/datapacks/:datapack_id/conversations[/:conversationId]`,
 * detail at `/datapacks/:datapack_id`, share at `/datapacks/:datapack_id/share`.
 */
export default function DatapacksPage() {
  const location = useLocation();
  // Shared, session-cached identity — one `/me` fetch app-wide.
  const { data: currentUser = null } = useCurrentUser();

  const prefix = `${ROUTES.datapacks}/`;
  if (location.pathname.startsWith(prefix)) {
    const rest = location.pathname.slice(prefix.length);
    const segments = rest.split("/").filter(Boolean);

    if (segments[0] === DATAPACK_SUB_ROUTE.new && segments[1] === DATAPACK_SUB_ROUTE.configure) {
      return <DatapackConfigureWizardPage isCreateFlow currentUser={currentUser} />;
    }

    if (segments[0] === DATAPACK_SUB_ROUTE.new && segments.length === 1) {
      return <Navigate to={datapackNewConfigurePath()} replace />;
    }

    const idEncoded = segments[0];
    if (idEncoded) {
      const datapackId = decodeURIComponent(idEncoded);
      if (!UUID_PATH_SEGMENT_RE.test(datapackId)) {
        return <Navigate to={ROUTES.datapacks} replace />;
      }
      if (segments[1] === DATAPACK_SUB_ROUTE.share) {
        return <DatapackSharePage datapackId={datapackId} currentUser={currentUser} />;
      }
      if (segments[1] === DATAPACK_SUB_ROUTE.edit && segments.length === 2) {
        return <DatapackEditPage datapackId={datapackId} currentUser={currentUser} />;
      }
      if (segments[1] === DATAPACK_SUB_ROUTE.configure) {
        return <DatapackConfigureWizardPage datapackId={datapackId} currentUser={currentUser} />;
      }
      if (segments[1] === DATAPACK_SUB_ROUTE.memorySummary && segments.length === 2) {
        return <DatapackMemorySummaryPage datapackId={datapackId} />;
      }
      if (segments[1] === DATAPACK_SUB_ROUTE.collectiveMemorySummary && segments.length === 2) {
        return <DatapackCollectiveMemorySummaryPage datapackId={datapackId} />;
      }
      if (segments[1] === DATAPACK_SUB_ROUTE.knowledge && segments.length === 2) {
        return <DatapackKnowledgeSummaryPage datapackId={datapackId} />;
      }
      if (segments[1] === DATAPACK_SUB_ROUTE.members && segments.length === 2) {
        return <DatapackMembersListPage datapackId={datapackId} />;
      }
      if (segments[1] === DATAPACK_SUB_ROUTE.clusters && segments.length === 2) {
        return <DatapackClustersPage datapackId={datapackId} />;
      }
      if (segments[1] === DATAPACK_SUB_ROUTE.learnings && segments.length === 2) {
        return <DatapackLearningsPage datapackId={datapackId} />;
      }
      if (segments[1] === DATAPACK_SUB_ROUTE.conversations && segments.length <= 3) {
        const conversationId = segments[2] ? decodeURIComponent(segments[2]) : undefined;
        return (
          <DatapackConversationsViewerPage
            datapackId={datapackId}
            conversationId={conversationId}
          />
        );
      }
      return <DatapackDetailPage datapackId={datapackId} currentUser={currentUser} />;
    }
  }

  return <DatapacksOverviewPage currentUser={currentUser} />;
}
