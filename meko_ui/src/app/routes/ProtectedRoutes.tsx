import { useLocation } from "react-router-dom";
import { ROUTES } from "./paths";
import Sidebar from "@/features/layout/Sidebar";
import DatapacksPage from "@/features/datapacks/pages/DatapacksPage";
import ObservePage from "@/features/observe/pages/ObservePage";
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

  return (
    <div className="flex h-screen w-full bg-background overflow-hidden text-foreground">
      <Sidebar />
      <div style={{ display: isObserve ? "none" : "contents" }}>
        <DatapacksPage />
      </div>
      <div style={{ display: isObserve ? "contents" : "none" }}>
        <ObservePage />
      </div>
    </div>
  );
}
