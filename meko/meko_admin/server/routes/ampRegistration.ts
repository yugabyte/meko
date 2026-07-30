import { Router } from "express";
import { apiServerGet, apiServerSend } from "../apiServerClient";

// AMP controller registration (global singleton), proxied to api_server — which
// owns the meko_system DB connection and migrations. meko_admin no longer holds
// its own DB pool for this, so there's a single source of DB truth and no
// separate connection to misconfigure. Password is written through api_server
// and never returned to the client.
const router = Router();

type Registration = { name: string; url: string; username?: string };

router.get("/", async (_req, res, next) => {
  try {
    // api_server returns null data when AMP isn't registered; surface that as a
    // 404 so the client treats it as "not registered".
    const data = await apiServerGet<Registration | null>("/amp-registration");
    if (!data) {
      res.status(404).json({ message: "AMP is not registered" });
      return;
    }
    res.json(data);
  } catch (e) {
    next(e);
  }
});

router.post("/", async (req, res, next) => {
  try {
    const b = (req.body ?? {}) as Record<string, unknown>;
    const data = await apiServerSend<Registration>("POST", "/amp-registration", {
      name: b.name,
      url: b.url,
      username: b.username,
      password: b.password,
    });
    res.status(201).json(data);
  } catch (e) {
    next(e);
  }
});

router.patch("/", async (req, res, next) => {
  try {
    const data = await apiServerSend<Registration>("PATCH", "/amp-registration", req.body ?? {});
    res.json(data);
  } catch (e) {
    next(e);
  }
});

router.delete("/", async (_req, res, next) => {
  try {
    const data = await apiServerSend<{ status: string }>("DELETE", "/amp-registration");
    res.json(data);
  } catch (e) {
    next(e);
  }
});

export default router;
