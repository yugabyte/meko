import { Navigate, useLocation, useSearchParams } from "react-router-dom";
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
import DatapackSharePage from "@/features/datapacks/pages/DatapackSharePage";
import DatapacksOverviewPage from "@/features/datapacks/pages/DatapacksOverviewPage";
import { useCurrentUser } from "@/hooks/useCurrentUser";

/**
 * Datapacks area: overview at `/datapacks`, new datapack wizard at `/datapacks/new/configure`, edit at `/datapacks/:name/edit`,
 * configure at `/datapacks/:name/configure`, learnings at `/datapacks/:name/learnings`,
 * memory-summary at `/datapacks/:name/memory-summary`,
 * collective-memory-summary at `/datapacks/:name/collective-memory-summary`,
 * knowledge at `/datapacks/:name/knowledge`,
 * members at `/datapacks/:name/members`,
 * conversations viewer at `/datapacks/:name/conversations[/:conversationId]`,
 * detail at `/datapacks/:name`, share at `/datapacks/:name/share`.
 */
export default function DatapacksPage() {
  const location = useLocation();
  const [searchParams] = useSearchParams();
  const routeDatapackId = (searchParams.get("datapack_id") ?? "").trim() || undefined;
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

    const nameEncoded = segments[0];
    if (nameEncoded) {
      const datapackName = decodeURIComponent(nameEncoded);
      if (segments[1] === DATAPACK_SUB_ROUTE.share) {
        return (
          <DatapackSharePage
            datapackName={datapackName}
            datapackId={routeDatapackId}
            currentUser={currentUser}
          />
        );
      }
      if (segments[1] === DATAPACK_SUB_ROUTE.edit && segments.length === 2) {
        return (
          <DatapackEditPage
            datapackName={datapackName}
            datapackId={routeDatapackId}
            currentUser={currentUser}
          />
        );
      }
      if (segments[1] === DATAPACK_SUB_ROUTE.configure) {
        return (
          <DatapackConfigureWizardPage
            datapackName={datapackName}
            datapackId={routeDatapackId}
            currentUser={currentUser}
          />
        );
      }
      if (segments[1] === DATAPACK_SUB_ROUTE.memorySummary && segments.length === 2) {
        return (
          <DatapackMemorySummaryPage datapackName={datapackName} datapackId={routeDatapackId} />
        );
      }
      if (segments[1] === DATAPACK_SUB_ROUTE.collectiveMemorySummary && segments.length === 2) {
        return (
          <DatapackCollectiveMemorySummaryPage
            datapackName={datapackName}
            datapackId={routeDatapackId}
          />
        );
      }
      if (segments[1] === DATAPACK_SUB_ROUTE.knowledge && segments.length === 2) {
        return (
          <DatapackKnowledgeSummaryPage datapackName={datapackName} datapackId={routeDatapackId} />
        );
      }
      if (segments[1] === DATAPACK_SUB_ROUTE.members && segments.length === 2) {
        return <DatapackMembersListPage datapackName={datapackName} datapackId={routeDatapackId} />;
      }
      if (segments[1] === DATAPACK_SUB_ROUTE.learnings && segments.length === 2) {
        return <DatapackLearningsPage datapackName={datapackName} datapackId={routeDatapackId} />;
      }
      if (segments[1] === DATAPACK_SUB_ROUTE.conversations && segments.length <= 3) {
        const conversationId = segments[2] ? decodeURIComponent(segments[2]) : undefined;
        return (
          <DatapackConversationsViewerPage
            datapackName={datapackName}
            datapackId={routeDatapackId}
            conversationId={conversationId}
          />
        );
      }
      return <DatapackDetailPage datapackName={datapackName} currentUser={currentUser} />;
    }
  }

  return <DatapacksOverviewPage currentUser={currentUser} />;
}
