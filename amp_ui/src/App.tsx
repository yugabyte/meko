// Top-level app: providers + routing.
//
// Public auth pages (/signin, /signup) have no chrome. Every
// authenticated route is nested under <AppLayout>, which supplies the
// sidebar + header shell; the page just renders its centre body. The
// per-section routes are generated from navConfig so the sidebar and
// the route table can never fall out of sync.

import { BrowserRouter, Navigate, Route, Routes } from "react-router-dom";
import type { ReactNode } from "react";
import { TooltipProvider } from "@/components/ui/tooltip";
import { Toaster } from "@/components/ui/sonner";
import { AuthProvider, useAuth } from "@/auth/AuthContext";
import { AppLayout } from "@/components/layout/AppLayout";
import { ROUTABLE_NAV } from "@/components/layout/navConfig";
import SignInPage from "@/pages/SignInPage";
import SignUpPage from "@/pages/SignUpPage";
import WelcomePage from "@/pages/WelcomePage";
import PlaceholderPage from "@/pages/PlaceholderPage";

/** Gate for authenticated-only routes. */
function RequireAuth({ children }: { children: ReactNode }) {
  const { isAuthenticated } = useAuth();
  return isAuthenticated ? <>{children}</> : <Navigate to="/signin" replace />;
}

/** Keep signed-in users out of the auth pages. */
function RedirectIfAuthed({ children }: { children: ReactNode }) {
  const { isAuthenticated } = useAuth();
  return isAuthenticated ? <Navigate to="/home" replace /> : <>{children}</>;
}

function AppRoutes() {
  const { isAuthenticated } = useAuth();
  return (
    <Routes>
      <Route
        path="/signin"
        element={
          <RedirectIfAuthed>
            <SignInPage />
          </RedirectIfAuthed>
        }
      />
      <Route
        path="/signup"
        element={
          <RedirectIfAuthed>
            <SignUpPage />
          </RedirectIfAuthed>
        }
      />

      {/* Authenticated shell — all nav targets render inside AppLayout. */}
      <Route
        element={
          <RequireAuth>
            <AppLayout />
          </RequireAuth>
        }
      >
        <Route path="/home" element={<WelcomePage />} />
        {ROUTABLE_NAV.filter((item) => item.to !== "/home").map((item) => (
          <Route
            key={item.to}
            path={item.to}
            element={<PlaceholderPage title={item.label} />}
          />
        ))}
      </Route>

      <Route
        path="*"
        element={<Navigate to={isAuthenticated ? "/home" : "/signin"} replace />}
      />
    </Routes>
  );
}

export default function App() {
  return (
    <AuthProvider>
      <TooltipProvider>
        <BrowserRouter>
          <AppRoutes />
        </BrowserRouter>
        <Toaster />
      </TooltipProvider>
    </AuthProvider>
  );
}
