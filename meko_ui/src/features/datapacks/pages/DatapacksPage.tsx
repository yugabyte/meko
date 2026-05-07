import { useEffect, useState } from "react";
import { Navigate, useLocation, useSearchParams } from "react-router-dom";
import { datapackNewConfigurePath, ROUTES } from "@/app/routes/paths";
import DatapackConfigureWizardPage from "@/features/datapacks/pages/DatapackConfigureWizardPage";
import DatapackConversationsViewerPage from "@/features/datapacks/pages/DatapackConversationsViewerPage";
import DatapackDetailPage from "@/features/datapacks/pages/DatapackDetailPage";
import DatapackEditPage from "@/features/datapacks/pages/DatapackEditPage";
import DatapackLearningsPage from "@/features/datapacks/pages/DatapackLearningsPage";
import DatapackMemorySummaryPage from "@/features/datapacks/pages/DatapackMemorySummaryPage";
import DatapackCollectiveMemorySummaryPage from "@/features/datapacks/pages/DatapackCollectiveMemorySummaryPage";
import DatapackSharePage from "@/features/datapacks/pages/DatapackSharePage";
import DatapacksOverviewPage from "@/features/datapacks/pages/DatapacksOverviewPage";
import { getCurrentUser, type CurrentUser } from "@/lib/mekoApi";

/**
 * Datapacks area: overview at `/datapacks`, new datapack wizard at `/datapacks/new/configure`, edit at `/datapacks/:name/edit`,
 * configure at `/datapacks/:name/configure`, learnings at `/datapacks/:name/learnings`,
 * memory-summary at `/datapacks/:name/memory-summary`,
 * collective-memory-summary at `/datapacks/:name/collective-memory-summary`,
 * conversations viewer at `/datapacks/:name/conversations[/:conversationId]`,
 * detail at `/datapacks/:name`, share at `/datapacks/:name/share`.
 */
export default function DatapacksPage() {
  const location = useLocation();
  const [searchParams] = useSearchParams();
  const routeDatapackId = (searchParams.get("datapack_id") ?? "").trim() || undefined;
  const [currentUser, setCurrentUser] = useState<CurrentUser | null>(null);

  useEffect(() => {
    let cancelled = false;
    void getCurrentUser()
      .then((user) => {
        if (!cancelled) setCurrentUser(user);
      })
      .catch(() => {
        if (!cancelled) setCurrentUser(null);
      });
    return () => {
      cancelled = true;
    };
  }, []);

  const prefix = `${ROUTES.datapacks}/`;
  if (location.pathname.startsWith(prefix)) {
    const rest = location.pathname.slice(prefix.length);
    const segments = rest.split("/").filter(Boolean);

    if (segments[0] === "new" && segments[1] === "configure") {
      return <DatapackConfigureWizardPage isCreateFlow currentUser={currentUser} />;
    }

    if (segments[0] === "new" && segments.length === 1) {
      return <Navigate to={datapackNewConfigurePath()} replace />;
    }

    const nameEncoded = segments[0];
    if (nameEncoded) {
      const datapackName = decodeURIComponent(nameEncoded);
      if (segments[1] === "share") {
        return <DatapackSharePage datapackName={datapackName} datapackId={routeDatapackId} currentUser={currentUser} />;
      }
      if (segments[1] === "edit" && segments.length === 2) {
        return <DatapackEditPage datapackName={datapackName} datapackId={routeDatapackId} currentUser={currentUser} />;
      }
      if (segments[1] === "configure") {
        return <DatapackConfigureWizardPage datapackName={datapackName} datapackId={routeDatapackId} currentUser={currentUser} />;
      }
      if (segments[1] === "memory-summary" && segments.length === 2) {
        return <DatapackMemorySummaryPage datapackName={datapackName} />;
      }
      if (segments[1] === "collective-memory-summary" && segments.length === 2) {
        return <DatapackCollectiveMemorySummaryPage datapackName={datapackName} />;
      }
      if (segments[1] === "learnings" && segments.length === 2) {
        return <DatapackLearningsPage datapackName={datapackName} />;
      }
      if (segments[1] === "conversations" && segments.length <= 3) {
        const conversationId = segments[2]
          ? decodeURIComponent(segments[2])
          : undefined;
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
