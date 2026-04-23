import { useLocation } from "react-router-dom";
import { ROUTES } from "./paths";
import Sidebar from "@/features/layout/Sidebar";
import DatapacksPage from "@/features/datapacks/pages/DatapacksPage";
import ObservePage from "@/features/observe/pages/ObservePage";
import HomePage from "@/features/home/pages/HomePage";
import OnboardingPage from "@/features/onboarding/pages/OnboardingPage";

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
  /** Datapacks UI for `/datapacks` and any other non-observe, non-home path (legacy deep links). */
  const isDatapacks = !isObserve && !isHome;

  return (
    <div className="flex h-screen w-full bg-background overflow-hidden text-foreground">
      <Sidebar />
      <div style={{ display: isHome ? "contents" : "none" }}>
        <HomePage />
      </div>
      <div style={{ display: isDatapacks ? "contents" : "none" }}>
        <DatapacksPage />
      </div>
      <div style={{ display: isObserve ? "contents" : "none" }}>
        <ObservePage />
      </div>
    </div>
  );
}
