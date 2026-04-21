import { useEffect, useState } from "react";
import { Switch, Route, useLocation } from "wouter";
import { queryClient } from "./lib/queryClient";
import { QueryClientProvider } from "@tanstack/react-query";
import { Toaster } from "@/components/ui/toaster";
import { TooltipProvider } from "@/components/ui/tooltip";
import NotFound from "@/pages/not-found";
import Datapacks from "@/pages/Datapacks";
import MekoAgent from "@/pages/MekoAgent";
import Onboarding from "@/pages/Onboarding";
import Observe from "@/pages/Observe";
import Sidebar from "@/components/Sidebar";
import SignIn from "@/pages/SignIn";
import SignUp from "@/pages/SignUp";
import { getCurrentUser } from "@/lib/mekoApi";

function Router() {
  const [location, navigate] = useLocation();
  const [authLoading, setAuthLoading] = useState(true);
  const [authenticated, setAuthenticated] = useState(false);
  const authRoutes = ["/", "/login", "/signin", "/signup"];
  const protectedPrefixes = ["/onboarding", "/agent", "/datapacks", "/observe-hub"];
  const isAuthRoute = authRoutes.includes(location);
  const isProtectedRoute = protectedPrefixes.some(
    (prefix) => location === prefix || location.startsWith(`${prefix}/`),
  );

  useEffect(() => {
    let mounted = true;
    setAuthLoading(true);

    getCurrentUser()
      .then(() => {
        if (!mounted) return;
        setAuthenticated(true);
      })
      .catch(() => {
        if (!mounted) return;
        setAuthenticated(false);
      })
      .finally(() => {
        if (!mounted) return;
        setAuthLoading(false);
      });
    return () => {
      mounted = false;
    };
  }, []);

  useEffect(() => {
    if (authLoading) return;
    if (authenticated && isAuthRoute) {
      navigate("/datapacks", { replace: true });
    }
    if (!authenticated && isProtectedRoute) {
      navigate("/login", { replace: true });
    }
  }, [authLoading, authenticated, isAuthRoute, isProtectedRoute, navigate]);

  useEffect(() => {
    const onAuthExpired = () => {
      setAuthenticated(false);
      setAuthLoading(false);
      navigate("/login", { replace: true });
    };
    window.addEventListener("meko:auth-expired", onAuthExpired);
    return () => {
      window.removeEventListener("meko:auth-expired", onAuthExpired);
    };
  }, [navigate]);

  if (authLoading) {
    return (
      <div className="min-h-screen flex items-center justify-center bg-background text-muted-foreground">
        Loading...
      </div>
    );
  }

  if (!authenticated) {
    if (isProtectedRoute) {
      return null;
    }
    return (
      <Switch>
        <Route path="/" component={SignIn} />
        <Route path="/login" component={SignIn} />
        <Route path="/signup" component={SignUp} />
        <Route path="/signin" component={SignIn} />
        <Route component={NotFound} />
      </Switch>
    );
  }

  if (isAuthRoute) {
    return null;
  }

  return (
    <div className="flex h-screen w-full bg-background overflow-hidden text-foreground">
      <Sidebar />
      <Switch>
        <Route path="/" component={Datapacks} />
        <Route path="/onboarding" component={Onboarding} />
        <Route path="/agent" component={MekoAgent} />
        <Route path="/datapacks" component={Datapacks} />
        <Route path="/observe-hub" component={Observe} />
        <Route component={NotFound} />
      </Switch>
    </div>
  );
}

function App() {
  return (
    <QueryClientProvider client={queryClient}>
      <TooltipProvider>
        <Toaster />
        <Router />
      </TooltipProvider>
    </QueryClientProvider>
  );
}

export default App;
