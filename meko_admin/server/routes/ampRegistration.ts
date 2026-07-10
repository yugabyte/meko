import { Router } from "express";
import { pool } from "../db";

// AMP controller registration (global singleton). The admin registers the AMP
// control-plane endpoint for this Meko deployment; api_server reads this row to
// reach AMP when provisioning databases inside datapacks. Written directly to
// meko_system.amp_registration (same DB the metrics route queries).
//
// NOTE: `password` is stored as-is for now (the AMP controller has no auth yet)
// and is never returned to the client. Move to a secret store / encrypt at rest
// before AMP enforces auth.
const router = Router();

router.get("/", async (_req, res) => {
  const { rows } = await pool.query<{ name: string; base_url: string; username: string }>(
    "SELECT name, base_url, username FROM meko_system.amp_registration WHERE singleton = TRUE",
  );
  if (rows.length === 0) {
    res.status(404).json({ message: "AMP is not registered" });
    return;
  }
  // Return username (not password) so the client can preserve it on update.
  res.json({ name: rows[0].name, url: rows[0].base_url, username: rows[0].username });
});

router.post("/", async (req, res) => {
  const body = (req.body ?? {}) as Record<string, unknown>;
  const url = typeof body.url === "string" ? body.url.trim() : "";
  if (!url) {
    res.status(400).json({ message: "url is required" });
    return;
  }
  const name = typeof body.name === "string" ? body.name.trim() : "";
  const username = typeof body.username === "string" ? body.username : "";
  const password = typeof body.password === "string" ? body.password : "";
  await pool.query(
    `INSERT INTO meko_system.amp_registration (singleton, name, base_url, username, password)
     VALUES (TRUE, $1, $2, $3, $4)
     ON CONFLICT (singleton) DO UPDATE
       SET name = EXCLUDED.name, base_url = EXCLUDED.base_url,
           username = EXCLUDED.username, password = EXCLUDED.password, updated_at = now()`,
    [name, url, username, password],
  );
  res.status(201).json({ name, url });
});

router.patch("/", async (req, res) => {
  const { rows } = await pool.query<{
    name: string;
    base_url: string;
    username: string;
    password: string;
  }>("SELECT name, base_url, username, password FROM meko_system.amp_registration WHERE singleton = TRUE");
  if (rows.length === 0) {
    res.status(404).json({ message: "AMP is not registered" });
    return;
  }
  const cur = rows[0];
  const body = (req.body ?? {}) as Record<string, unknown>;
  // Only string fields override the current value; anything else (incl. an
  // explicit null) leaves it untouched — never coerce null to the "null" string.
  const name = typeof body.name === "string" ? body.name.trim() : cur.name;
  const url = typeof body.url === "string" ? body.url.trim() : cur.base_url;
  const username = typeof body.username === "string" ? body.username : cur.username;
  const password = typeof body.password === "string" ? body.password : cur.password;
  if (!url) {
    res.status(400).json({ message: "url cannot be empty" });
    return;
  }
  await pool.query(
    `UPDATE meko_system.amp_registration
     SET name = $1, base_url = $2, username = $3, password = $4, updated_at = now()
     WHERE singleton = TRUE`,
    [name, url, username, password],
  );
  res.json({ name, url });
});

router.delete("/", async (_req, res) => {
  await pool.query("DELETE FROM meko_system.amp_registration WHERE singleton = TRUE");
  res.json({ status: "unregistered" });
});

export default router;
