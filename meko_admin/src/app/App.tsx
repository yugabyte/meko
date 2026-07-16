import { QueryClientProvider } from "@tanstack/react-query";
import { BrowserRouter, Routes, Route, Navigate } from "react-router-dom";
import { Toaster } from "@/components/ui/sonner";
import { queryClient } from "@/lib/queryClient";
import { AuthProvider, useAuth } from "@/lib/authContext";
import { AdminSidebar } from "@/features/layout/AdminSidebar";
import { AdminHeader } from "@/features/layout/AdminHeader";
import { DataPlanePage } from "@/features/dataplane/pages/DataPlanePage";
import { MetricsPage } from "@/features/metrics/pages/MetricsPage";
import { ExecutiveDashboardPage } from "@/features/exec/pages/ExecutiveDashboardPage";
import { UsersPage } from "@/features/users/pages/UsersPage";
import { AmpRegistrationPage } from "@/features/amp/pages/AmpRegistrationPage";
import { LoginPage } from "@/features/auth/LoginPage";
import { isAmpAdminEnabled } from "@/lib/amp";
import { ROUTES } from "./routes/paths";

function AppShell() {
  const { user, isLoading } = useAuth();

  if (isLoading) {
    return <div className="flex h-screen items-center justify-center text-sm text-muted-foreground">Loading…</div>;
  }

  if (!user) {
    return (
      <Routes>
        <Route path={ROUTES.login} element={<LoginPage />} />
        <Route path="*" element={<Navigate to={ROUTES.login} replace />} />
      </Routes>
    );
  }

  return (
    <div className="flex h-screen w-full overflow-hidden bg-background">
      <AdminSidebar />
      <div className="flex flex-1 flex-col overflow-hidden">
        <AdminHeader />
        <main className="flex flex-1 flex-col overflow-hidden">
          <Routes>
            <Route path={ROUTES.dataPlane} element={<DataPlanePage />} />
            <Route path={ROUTES.users} element={<UsersPage />} />
            <Route path={ROUTES.metrics} element={<MetricsPage />} />
            <Route path={ROUTES.executive} element={<ExecutiveDashboardPage />} />
            {isAmpAdminEnabled() && (
              <Route path={ROUTES.ampRegistration} element={<AmpRegistrationPage />} />
            )}
            <Route path="*" element={<Navigate to={ROUTES.metrics} replace />} />
          </Routes>
        </main>
      </div>
    </div>
  );
}

export default function App() {
  return (
    <QueryClientProvider client={queryClient}>
      <Toaster />
      <BrowserRouter>
        <AuthProvider>
          <AppShell />
        </AuthProvider>
      </BrowserRouter>
    </QueryClientProvider>
  );
}
