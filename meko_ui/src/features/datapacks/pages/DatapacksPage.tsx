import { useEffect, useState } from "react";
import { useLocation } from "react-router-dom";
import { ROUTES } from "@/app/routes/paths";
import DatapackDetailPage from "@/features/datapacks/pages/DatapackDetailPage";
import DatapacksOverviewPage from "@/features/datapacks/pages/DatapacksOverviewPage";
import { getCurrentUser, type CurrentUser } from "@/lib/mekoApi";

/**
 * Datapacks area: list/overview at `/datapacks`, per-datapack workspace at `/datapacks/:name`.
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
    const segment = rest.split("/")[0];
    if (segment) {
      return <DatapackDetailPage datapackName={decodeURIComponent(segment)} />;
    }
  }

  return <DatapacksOverviewPage currentUser={currentUser} />;
}
