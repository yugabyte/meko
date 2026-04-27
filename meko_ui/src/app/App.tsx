import { useEffect, useState } from "react";
import axios from "axios";
import {
  BrowserRouter,
  Navigate,
  useLocation,
  useNavigate,
} from "react-router-dom";
import { QueryClientProvider } from "@tanstack/react-query";
import { Toaster } from "@/components/ui/toaster";
import { TooltipProvider } from "@/components/ui/tooltip";
import { queryClient } from "@/lib/queryClient";
import PublicRoutes from "@/app/routes/PublicRoutes";
import ProtectedRoutes from "@/app/routes/ProtectedRoutes";
import { isAuthRoute, isOnboardingRoute, isProtectedRoute } from "@/app/routes/guards";
import { ROUTES } from "@/app/routes/paths";
import { useOnboardingGuard } from "@/app/routes/OnboardingGuard";
import {
  readOnboardingStatusCache,
  useAuthBootstrap,
} from "@/features/auth/hooks/useAuthBootstrap";
import { useAuthExpiredRedirect } from "@/features/auth/hooks/useAuthExpiredRedirect";
import { useOnboardingStatusSync } from "@/features/onboarding/hooks/useOnboardingStatusSync";
import { addAppEventListener, removeAppEventListener } from "@/lib/browserEvents";
import {
  AUTH_LOGIN_SUCCESS_EVENT,
  AUTH_LOGOUT_EVENT,
  getCurrentUser,
  ONBOARDING_STATUS_CACHE_KEY,
  resetAuthExpiredState,
} from "@/lib/mekoApi";


function AppRouter() {
  const location = useLocation();
  const navigate = useNavigate();
  const [authLoading, setAuthLoading] = useState(true);
  const [authenticated, setAuthenticated] = useState(false);
  const [hasCompletedOnboarding, setHasCompletedOnboarding] = useState<boolean>(
    () => readOnboardingStatusCache() ?? false,
  );

  useAuthBootstrap({
    setAuthLoading,
    setAuthenticated,
    setHasCompletedOnboarding,
  });

  useAuthExpiredRedirect({
    navigate,
    redirectTo: ROUTES.login,
    setAuthenticated,
    setAuthLoading,
  });

  useOnboardingStatusSync({ setHasCompletedOnboarding });

  // Keep auth state in sync immediately after successful login.
  // This avoids being stuck on auth pages without forcing a full reload.
  useEffect(() => {
    let controller: AbortController | null = null;

    const syncAuthAfterLogin = async () => {
      // Cancel any in-flight sync triggered by a previous login event so we
      // don't apply stale results over a newer one.
      controller?.abort();
      controller = new AbortController();
      const signal = controller.signal;

      setAuthLoading(true);
      try {
        const user = await getCurrentUser({ signal });
        if (signal.aborted) return;
        setAuthenticated(true);
        setHasCompletedOnboarding(user.has_completed_onboarding);
        localStorage.setItem(
          ONBOARDING_STATUS_CACHE_KEY,
          String(user.has_completed_onboarding),
        );
      } catch (err) {
        if (signal.aborted || axios.isCancel(err)) return;
        setAuthenticated(false);
        localStorage.removeItem(ONBOARDING_STATUS_CACHE_KEY);
      } finally {
        if (!signal.aborted) {
          setAuthLoading(false);
        }
      }
    };

    addAppEventListener(AUTH_LOGIN_SUCCESS_EVENT, syncAuthAfterLogin);
    return () => {
      removeAppEventListener(AUTH_LOGIN_SUCCESS_EVENT, syncAuthAfterLogin);
      controller?.abort();
    };
  }, [setAuthLoading, setAuthenticated, setHasCompletedOnboarding]);

  // Clear client-side auth state immediately after an explicit logout so the
  // routing guards don't momentarily see `authenticated=true` and redirect
  // through /onboarding before the next bootstrap runs.
  useEffect(() => {
    const onLogout = () => {
      setAuthenticated(false);
      setHasCompletedOnboarding(false);
      setAuthLoading(false);
      resetAuthExpiredState();
    };

    addAppEventListener(AUTH_LOGOUT_EVENT, onLogout);
    return () => {
      removeAppEventListener(AUTH_LOGOUT_EVENT, onLogout);
    };
  }, [setAuthLoading, setAuthenticated, setHasCompletedOnboarding]);

  useOnboardingGuard({
    location: location.pathname,
    authLoading,
    authenticated,
    hasCompletedOnboarding,
    navigate,
  });

  if (authLoading) {
    return (
      <div className="min-h-screen flex items-center justify-center bg-background text-muted-foreground">
        Loading...
      </div>
    );
  }

  if (!authenticated) {
    if (isProtectedRoute(location.pathname)) {
      return <Navigate to={ROUTES.login} replace />;
    }
    return <PublicRoutes />;
  }

  if (isAuthRoute(location.pathname)) {
    return (
      <Navigate
        to={ROUTES.onboarding}
        replace
      />
    );
  }

  return <ProtectedRoutes isOnboardingRoute={isOnboardingRoute(location.pathname)} />;
}

export default function App() {
  return (
    <QueryClientProvider client={queryClient}>
      <TooltipProvider>
        <Toaster />
        <BrowserRouter>
          <AppRouter />
        </BrowserRouter>
      </TooltipProvider>
    </QueryClientProvider>
  );
}
