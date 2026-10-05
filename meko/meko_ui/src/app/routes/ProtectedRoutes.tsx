import { Navigate, useLocation } from "react-router-dom";
import { ROUTES, isProjectsPath } from "./paths";
import Sidebar from "@/features/layout/Sidebar";
import { KbImportStatusBanner } from "@/features/layout/KbImportStatusBanner";
import DatapacksPage from "@/features/datapacks/pages/DatapacksPage";
import ObservePage from "@/features/observe/pages/ObservePage";
import HomePage from "@/features/home/pages/HomePage";
import OnboardingPage from "@/features/onboarding/pages/OnboardingPage";
import WorkbenchLMPage from "@/features/workbench-lm/pages/WorkbenchLMPage";
import SettingsPage from "@/features/settings/pages/SettingsPage";
import UsagePage from "@/features/settings/pages/UsagePage";
import BillingPage from "@/features/billing/pages/BillingPage";
import MarketplaceCompletePage from "@/features/billing/pages/MarketplaceCompletePage";
import LabsSection from "@/features/labs/pages/LabsSection";
import LabRunnerPage from "@/features/labs/pages/LabRunnerPage";
import { parseLabRoute } from "@/features/labs/lib/parseLabRoute";
import { isLabsEnabled, isLabsPath } from "@/lib/labs";
import ProjectsSection from "@/features/projects/pages/ProjectsSection";
import { useProjectsEnabled } from "@/lib/projectsFeature";

type Props = {
  isOnboardingRoute: boolean;
};

export default function ProtectedRoutes({ isOnboardingRoute }: Props) {
  const { pathname } = useLocation();
  const isLabRunner = parseLabRoute(pathname)?.isRunner ?? false;
  // Projects gate. A hook rather than a bare function like isLabsEnabled,
  // because the per-browser `?projects=true|false` override is latched in
  // localStorage and has to re-render this tree when it flips.
  const projectsEnabled = useProjectsEnabled();

  // MEKO_ENABLE_PROJECTS off (and no per-browser override): bounce every
  // /projects route, for the same reason Labs does below — a bookmark should
  // land somewhere real rather than render Datapacks under a /projects URL.
  if (isProjectsPath(pathname) && !projectsEnabled) {
    return <Navigate to={ROUTES.datapacks} replace />;
  }

  // MEKO_ENABLE_LABS off: bounce every /labs route, before the runner's
  // early return below can take over the viewport. A redirect rather than a
  // silent fallthrough: a bookmark or a shared link should land somewhere
  // real, not render Datapacks under a /labs URL. Sidebar hides the entry on
  // the same flag.
  if (isLabsPath(pathname) && !isLabsEnabled()) {
    return <Navigate to={ROUTES.datapacks} replace />;
  }

  // The lab runner takes over the whole viewport -- no Sidebar, no shared
  // shell -- so users get a focused, distraction-free lab experience with
  // its own top bar (LabRunnerPage owns its own exit-to-list action). It
  // self-guards internally (an invalid :labId redirects to the list), so no
  // extra logic is needed here beyond the early return itself.
  if (isLabRunner) {
    return <LabRunnerPage />;
  }

  if (isOnboardingRoute) {
    // Keep the same shell (Sidebar + workspace) as the rest of the
    // app so users see the standard navigation while they're picking
    // an onboarding path. Previously this rendered full-screen with
    // no sidebar; the change makes the onboarding experience
    // continuous with the post-onboarding workspace.
    // `h-screen` resolves to 100vh, which on iOS Safari does NOT shrink
    // when the on-screen keyboard slides up — the bottom of the page
    // (incl. the chat input) sits behind the keyboard. `h-dvh` (dynamic
    // viewport height) does shrink, so the chat input stays visible
    // above the keyboard. Falls back to h-screen if dvh isn't supported.
    // Desktop reads identical height since dvh == vh outside keyboard.
    return (
      <div className="flex h-screen h-dvh w-full bg-background overflow-hidden text-foreground">
        <Sidebar />
        <div className="flex min-h-0 min-w-0 flex-1 flex-col overflow-y-auto">
          <OnboardingPage />
        </div>
      </div>
    );
  }

  const isObserve = pathname === ROUTES.observeHub || pathname.startsWith(`${ROUTES.observeHub}/`);
  const isConnect = pathname === ROUTES.connect || pathname.startsWith(`${ROUTES.connect}/`);
  const isWorkbenchLM =
    pathname === ROUTES.workbenchLm || pathname.startsWith(`${ROUTES.workbenchLm}/`);
  const isSettings = pathname === ROUTES.settings || pathname.startsWith(`${ROUTES.settings}/`);
  const isUsage = pathname === ROUTES.usage || pathname.startsWith(`${ROUTES.usage}/`);

  const isBilling = pathname === ROUTES.billing || pathname.startsWith(`${ROUTES.billing}/`);
  const isLabsRoute = isLabsPath(pathname);
  const isLabsListOrDetail = isLabsRoute && !isLabRunner;
  const isProjects = isProjectsPath(pathname);
  const isMarketplaceComplete = pathname === ROUTES.marketplaceComplete;

  /** Datapacks UI for `/datapacks` and legacy deep links; excludes connect, observe, workbench-lm, settings, usage, billing, marketplace-complete, labs, projects. */
  const isDatapacks =
    !isObserve &&
    !isConnect &&
    !isWorkbenchLM &&
    !isSettings &&
    !isUsage &&
    !isBilling &&
    !isMarketplaceComplete &&
    !isLabsRoute &&
    !isProjects;

  // See onboarding-shell comment above re: h-dvh for iOS keyboard.
  return (
    <div className="flex h-screen h-dvh w-full bg-background overflow-hidden text-foreground">
      <Sidebar />
      <div className="flex min-h-0 min-w-0 flex-1 flex-col overflow-hidden">
        <KbImportStatusBanner />
        <div className="flex min-h-0 min-w-0 flex-1 flex-col overflow-hidden">
          <div
            className="min-h-0 flex-1 overflow-hidden"
            style={{ display: isConnect ? "contents" : "none" }}
          >
            <HomePage />
          </div>
          {/*
            WorkbenchLM is the ONE page we deliberately conditionally
            mount. The other four (Home / Settings / Datapacks / Observe)
            stay mounted under `display: none` to preserve in-memory
            state across navigation. WorkbenchLM previously did the
            same with an `isRouteActive` prop meant to pause its
            background polling (memories / promoted-memories /
            documents), but several effects still tick — so we
            unmount it instead. Trade-off: in-memory state (chat
            scroll position, expanded panels) resets on tab return,
            but the API stops getting hit from a hidden tree.

            No wrapper div here on purpose: the other pages render
            their wrappers with `display: contents` so the page
            itself is a direct flex child of this column. Adding a
            real wrapper would inject an extra layout level and
            break the page's internal `flex-1` height chain (the
            chat composer would float mid-screen instead of pinning
            to the bottom).
          */}
          {isWorkbenchLM ? <WorkbenchLMPage isRouteActive={true} /> : null}
          <div
            className="min-h-0 flex-1 overflow-hidden"
            style={{ display: isSettings ? "contents" : "none" }}
          >
            <SettingsPage />
          </div>
          {/* Usage — conditionally mounted (not display-toggled) so the
              tracker's refetchOnMount:"always" refetches on every /usage visit,
              and nothing is fetched until the page is opened. UsagePage owns its
              own flex-1 height chain, so no wrapper is needed. */}
          {isUsage ? <UsagePage /> : null}
          <div
            className="min-h-0 flex-1 overflow-hidden"
            style={{ display: isBilling ? "contents" : "none" }}
          >
            <BillingPage />
          </div>
          {isMarketplaceComplete ? <MarketplaceCompletePage /> : null}
          <div
            className="min-h-0 flex-1 overflow-hidden"
            style={{ display: isLabsListOrDetail ? "contents" : "none" }}
          >
            <LabsSection />
          </div>
          {/* Projects — conditionally mounted rather than display-toggled, so
              a workspace with the flag off never issues /projects requests and
              the tree queries stop the moment the user navigates away. */}
          {isProjects ? <ProjectsSection /> : null}
          <div
            className="min-h-0 flex-1 overflow-hidden"
            style={{ display: isDatapacks ? "contents" : "none" }}
          >
            <DatapacksPage />
          </div>
          <div
            className="min-h-0 flex-1 overflow-hidden"
            style={{ display: isObserve ? "contents" : "none" }}
          >
            <ObservePage />
          </div>
        </div>
      </div>
    </div>
  );
}
