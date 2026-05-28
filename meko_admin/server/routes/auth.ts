import { Router } from "express";
import { loginWithCognito, completeNewPassword, verifyAccessToken } from "../adminAuth";

const router = Router();

router.post("/login", async (req, res) => {
  const { email, password } = req.body as { email?: string; password?: string };
  if (!email || !password) {
    res.status(400).json({ error: "Email and password are required" });
    return;
  }

  try {
    const result = await loginWithCognito(email, password);

    if (result.type === "new_password_required") {
      res.json({
        challenge: "NEW_PASSWORD_REQUIRED",
        session: result.session,
        email: result.email,
      });
      return;
    }

    res.json({
      accessToken: result.tokens.AccessToken,
      expiresIn: result.tokens.ExpiresIn,
    });
  } catch (err: unknown) {
    const name = (err as { name?: string })?.name ?? "";
    const rawMessage = (err as { message?: string })?.message ?? "";
    console.error(`[auth/login] ${name}: ${rawMessage}`);
    const message =
      name === "NotAuthorizedException"
        ? "Incorrect email or password"
        : name === "UserNotFoundException"
          ? "No account found with this email"
          : name === "UserNotConfirmedException"
            ? "Account not confirmed — check your email for a verification link"
            : "Login failed. Please try again.";
    res.status(401).json({ error: message });
  }
});

router.post("/new-password", async (req, res) => {
  const { email, newPassword, session, givenName, familyName } = req.body as {
    email?: string;
    newPassword?: string;
    session?: string;
    givenName?: string;
    familyName?: string;
  };
  if (!email || !newPassword || !session) {
    res.status(400).json({ error: "Email, new password, and session are required" });
    return;
  }

  try {
    const tokens = await completeNewPassword(email, newPassword, session, givenName, familyName);
    res.json({ accessToken: tokens.AccessToken, expiresIn: tokens.ExpiresIn });
  } catch (err: unknown) {
    res.status(400).json({ error: (err as { message?: string })?.message ?? "Failed to set new password" });
  }
});

router.get("/me", async (req, res) => {
  const header = req.headers.authorization;
  if (!header?.startsWith("Bearer ")) {
    res.status(401).json({ error: "Unauthorized" });
    return;
  }

  const user = await verifyAccessToken(header.slice(7));
  if (!user) {
    res.status(401).json({ error: "Invalid or expired token" });
    return;
  }

  res.json(user);
});

export default router;
