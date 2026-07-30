import { type Request, type Response, type NextFunction } from "express";
import { verifyAccessToken, type AdminUser } from "../adminAuth";

export interface AuthenticatedRequest extends Request {
  adminUser: AdminUser;
}

export async function requireAdminAuth(
  req: Request,
  res: Response,
  next: NextFunction,
): Promise<void> {
  const header = req.headers.authorization;
  if (!header?.startsWith("Bearer ")) {
    res.status(401).json({ error: "Unauthorized" });
    return;
  }

  const token = header.slice(7);
  const user = await verifyAccessToken(token);

  if (!user) {
    res.status(401).json({ error: "Invalid or expired token" });
    return;
  }

  (req as AuthenticatedRequest).adminUser = user;
  next();
}
