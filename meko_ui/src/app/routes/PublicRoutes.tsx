import { Navigate, Route, Routes } from "react-router-dom";
import { ROUTES } from "./paths";
import SignInPage from "@/features/auth/pages/SignInPage";
import SignUpPage from "@/features/auth/pages/SignUpPage";
import ForgotPasswordPage from "@/features/auth/pages/ForgotPasswordPage";
import NotFoundPage from "@/features/system/pages/NotFoundPage";

export default function PublicRoutes() {
  return (
    <Routes>
      <Route path={ROUTES.root} element={<SignInPage />} />
      <Route path={ROUTES.login} element={<Navigate to={ROUTES.signin} replace />} />
      <Route path={ROUTES.signup} element={<SignUpPage />} />
      <Route path={ROUTES.forgotPassword} element={<ForgotPasswordPage />} />
      <Route path={ROUTES.signin} element={<SignInPage />} />
      <Route path="*" element={<NotFoundPage />} />
    </Routes>
  );
}
