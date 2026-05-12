import { useLocation } from "react-router-dom";
import { ROUTES } from "./paths";
import Sidebar from "@/features/layout/Sidebar";
import { KbImportStatusBanner } from "@/features/layout/KbImportStatusBanner";
import DatapacksPage from "@/features/datapacks/pages/DatapacksPage";
import ObservePage from "@/features/observe/pages/ObservePage";
import HomePage from "@/features/home/pages/HomePage";
import OnboardingPage from "@/features/onboarding/pages/OnboardingPage";
import PlaygroundPage from "@/features/playground/pages/PlaygroundPage";
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
  const isPlayground =
    pathname === ROUTES.playground || pathname.startsWith(`${ROUTES.playground}/`);
  const isSettings =
    pathname === ROUTES.settings || pathname.startsWith(`${ROUTES.settings}/`);
  /** Datapacks UI for `/datapacks` and legacy deep links; excludes home, observe, playground, settings. */
  const isDatapacks = !isObserve && !isHome && !isPlayground && !isSettings;

  return (
    <div className="flex h-screen w-full bg-background overflow-hidden text-foreground">
      <Sidebar />
      <div className="flex min-h-0 min-w-0 flex-1 flex-col overflow-hidden">
        <KbImportStatusBanner />
        <div className="flex min-h-0 min-w-0 flex-1 flex-col overflow-hidden">
          <div className="min-h-0 flex-1 overflow-hidden" style={{ display: isHome ? "contents" : "none" }}>
            <HomePage />
          </div>
          <div className="min-h-0 flex-1 overflow-hidden" style={{ display: isPlayground ? "contents" : "none" }}>
            <PlaygroundPage />
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
