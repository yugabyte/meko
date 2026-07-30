// Validates a single email string for the datapack “invite by email” field (no comma-separated lists).
// Returns a normalized address or a user-facing error message used for inline + toast feedback.
export type ParsedInviteEmail = { ok: true; email: string } | { ok: false; message: string };

export function parseSingleInviteEmail(raw: string): ParsedInviteEmail {
  const t = raw.trim();
  if (!t) {
    return { ok: false, message: "Enter an email address." };
  }
  if (/[,;]/.test(t)) {
    return { ok: false, message: "Enter one email address at a time." };
  }
  const single = t.replace(/\s+/g, " ").trim();
  if (/\s/.test(single)) {
    return { ok: false, message: "Enter a single email address with no spaces." };
  }
  // Practical single-address check (not full RFC)
  if (!/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(single)) {
    return { ok: false, message: "Enter a valid email address." };
  }
  return { ok: true, email: single };
}
