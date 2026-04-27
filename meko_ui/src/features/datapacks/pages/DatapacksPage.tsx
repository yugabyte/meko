import { useEffect, useState } from "react";
import { useLocation } from "react-router-dom";
import { ROUTES } from "@/app/routes/paths";
import DatapackConfigureWizardPage from "@/features/datapacks/pages/DatapackConfigureWizardPage";
import DatapackDetailPage from "@/features/datapacks/pages/DatapackDetailPage";
import DatapackLearningsPage from "@/features/datapacks/pages/DatapackLearningsPage";
import DatapackNewPage from "@/features/datapacks/pages/DatapackNewPage";
import DatapackSharePage from "@/features/datapacks/pages/DatapackSharePage";
import DatapacksOverviewPage from "@/features/datapacks/pages/DatapacksOverviewPage";
import { getCurrentUser, type CurrentUser } from "@/lib/mekoApi";

/**
 * Datapacks area: overview at `/datapacks`, new at `/datapacks/new`, configure at `/datapacks/:name/configure`,
 * detail at `/datapacks/:name`, share at `/datapacks/:name/share`.
 */
export default function DatapacksPage() {
  const location = useLocation();
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

    if (segments[0] === "new" && segments.length === 1) {
      return <DatapackNewPage currentUser={currentUser as CurrentUser} />;
    }

    const nameEncoded = segments[0];
    if (nameEncoded) {
      const datapackName = decodeURIComponent(nameEncoded);
      if (segments[1] === "share") {
        return <DatapackSharePage datapackName={datapackName} currentUser={currentUser} />;
      }
      if (segments[1] === "configure") {
        return <DatapackConfigureWizardPage datapackName={datapackName} />;
      }
      if (segments[1] === "learnings" && segments.length === 2) {
        return <DatapackLearningsPage datapackName={datapackName} />;
      }
      return <DatapackDetailPage datapackName={datapackName} currentUser={currentUser} />;
    }
  }

  return <DatapacksOverviewPage currentUser={currentUser} />;
}
