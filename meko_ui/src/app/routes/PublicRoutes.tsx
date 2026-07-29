import { Navigate, Route, Routes } from "react-router-dom";
import { ROUTES } from "./paths";
import SignInPage from "@/features/auth/pages/SignInPage";
import SignUpPage from "@/features/auth/pages/SignUpPage";
import ForgotPasswordPage from "@/features/auth/pages/ForgotPasswordPage";
import CompleteNewPasswordPage from "@/features/auth/pages/CompleteNewPasswordPage";
import NotFoundPage from "@/features/system/pages/NotFoundPage";
import MarketplaceSignInBouncePage from "@/features/billing/pages/MarketplaceSignInBouncePage";

export default function PublicRoutes() {
  return (
    <Routes>
      <Route path={ROUTES.root} element={<SignInPage />} />
      <Route path={ROUTES.login} element={<Navigate to={ROUTES.signin} replace />} />
      <Route path={ROUTES.signup} element={<SignUpPage />} />
      <Route path={ROUTES.forgotPassword} element={<ForgotPasswordPage />} />
      <Route path={ROUTES.completeNewPassword} element={<CompleteNewPasswordPage />} />
      <Route path={ROUTES.signin} element={<SignInPage />} />
      {/* Public-side landing for the AWS Marketplace fulfillment
          redirect. Bounces the not-yet-signed-in buyer to /signup
          while carrying the ?t=<token> through return_to so the
          authed side of the flow can finalize the entitlement. */}
      <Route path={ROUTES.marketplaceComplete} element={<MarketplaceSignInBouncePage />} />
      <Route path="*" element={<NotFoundPage />} />
    </Routes>
  );
}
