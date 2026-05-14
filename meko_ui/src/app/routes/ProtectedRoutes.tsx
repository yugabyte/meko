import { useLocation } from "react-router-dom";
import { ROUTES } from "./paths";
import Sidebar from "@/features/layout/Sidebar";
import { KbImportStatusBanner } from "@/features/layout/KbImportStatusBanner";
import DatapacksPage from "@/features/datapacks/pages/DatapacksPage";
import ObservePage from "@/features/observe/pages/ObservePage";
import HomePage from "@/features/home/pages/HomePage";
import OnboardingPage from "@/features/onboarding/pages/OnboardingPage";
import WorkbenchLMPage from "@/features/workbench-lm/pages/WorkbenchLMPage";
import SettingsPage from "@/features/settings/pages/SettingsPage";

type Props = {
  isOnboardingRoute: boolean;
};

export default function ProtectedRoutes({ isOnboardingRoute }: Props) {
  const { pathname } = useLocation();

  if (isOnboardingRoute) {
    return (
      <div className="min-h-screen w-full bg-background text-foreground overflow-y-auto">
        <OnboardingPage />
      </div>
    );
  }

  const isObserve = pathname === ROUTES.observeHub || pathname.startsWith(`${ROUTES.observeHub}/`);
  const isHome = pathname === ROUTES.home || pathname.startsWith(`${ROUTES.home}/`);
  const isWorkbenchLM =
    pathname === ROUTES.workbenchLm || pathname.startsWith(`${ROUTES.workbenchLm}/`);
  const isSettings =
    pathname === ROUTES.settings || pathname.startsWith(`${ROUTES.settings}/`);
  /** Datapacks UI for `/datapacks` and legacy deep links; excludes home, observe, workbench-lm, settings. */
  const isDatapacks = !isObserve && !isHome && !isWorkbenchLM && !isSettings;

  return (
    <div className="flex h-screen w-full bg-background overflow-hidden text-foreground">
      <Sidebar />
      <div className="flex min-h-0 min-w-0 flex-1 flex-col overflow-hidden">
        <KbImportStatusBanner />
        <div className="flex min-h-0 min-w-0 flex-1 flex-col overflow-hidden">
          <div className="min-h-0 flex-1 overflow-hidden" style={{ display: isHome ? "contents" : "none" }}>
            <HomePage />
          </div>
          <div className="min-h-0 flex-1 overflow-hidden" style={{ display: isWorkbenchLM ? "contents" : "none" }}>
            {/*
              All five pages stay mounted (toggled via `display: none`) to
              preserve in-memory state across navigation. WorkbenchLMPage
              runs background polling on /memories, /promoted-memories,
              and /documents — passing `isRouteActive` lets it pause those
              intervals while the user is on another route. Without this
              the page would keep hammering the API from a hidden tree.
            */}
            <WorkbenchLMPage isRouteActive={isWorkbenchLM} />
          </div>
          <div className="min-h-0 flex-1 overflow-hidden" style={{ display: isSettings ? "contents" : "none" }}>
            <SettingsPage />
          </div>
          <div className="min-h-0 flex-1 overflow-hidden" style={{ display: isDatapacks ? "contents" : "none" }}>
            <DatapacksPage />
          </div>
          <div className="min-h-0 flex-1 overflow-hidden" style={{ display: isObserve ? "contents" : "none" }}>
            <ObservePage />
          </div>
        </div>
      </div>
    </div>
  );
}
