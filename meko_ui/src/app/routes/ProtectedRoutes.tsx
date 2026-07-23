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
import BillingPage from "@/features/billing/pages/BillingPage";
import AdminPage from "@/features/admin/pages/AdminPage";

type Props = {
  isOnboardingRoute: boolean;
};

export default function ProtectedRoutes({ isOnboardingRoute }: Props) {
  const { pathname } = useLocation();

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

  const isBilling = pathname === ROUTES.billing || pathname.startsWith(`${ROUTES.billing}/`);
  // Admin console (flag-gated). Conditionally mounted like WorkbenchLM so its
  // /admin/* data fetches never tick while the user is elsewhere.
  // Mount AdminPage for ANY /admin route (not gated by the flag here): AdminPage
  // self-guards and redirects to Datapacks when the view is disabled, so a
  // direct URL to /admin with the flag/opt-in off actually bounces — rather than
  // rendering Datapacks content under a stale /admin URL.
  const onAdminRoute = pathname === ROUTES.admin || pathname.startsWith(`${ROUTES.admin}/`);

  /** Datapacks UI for `/datapacks` and legacy deep links; excludes connect, observe, workbench-lm, settings, admin. */
  const isDatapacks =
    !isObserve && !isConnect && !isWorkbenchLM && !isSettings && !isBilling && !onAdminRoute;

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
          {/* Admin console — conditionally mounted (not display-toggled) so it
              only fetches /admin/* data while actually being viewed. */}
          {onAdminRoute ? <AdminPage /> : null}
          <div
            className="min-h-0 flex-1 overflow-hidden"
            style={{ display: isSettings ? "contents" : "none" }}
          >
            <SettingsPage />
          </div>
          <div
            className="min-h-0 flex-1 overflow-hidden"
            style={{ display: isBilling ? "contents" : "none" }}
          >
            <BillingPage />
          </div>
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
