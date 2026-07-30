// Post-auth redirect helper. Callers pass the raw `?return_to=` value
// from the URL; we hand back either a safe same-origin path or "".
//
// Guard rails prevent open redirects: only allow relative paths that
// start with a single "/", no "//" (protocol-relative), no scheme,
// no whitespace. Anything suspicious returns "" so the caller falls
// back to its default landing route.

export function safeReturnTo(raw: string | null | undefined): string {
  if (!raw) return "";
  const trimmed = raw.trim();
  if (!trimmed.startsWith("/")) return "";
  if (trimmed.startsWith("//")) return "";
  if (/^\s/.test(trimmed) || /\s$/.test(trimmed)) return "";
  if (trimmed.includes("\\")) return "";
  // Reject anything looking like a URL scheme (mailto:, javascript:, etc.)
  if (/^\/?[a-z][a-z0-9+.-]*:/i.test(trimmed.slice(1))) return "";
  return trimmed;
}
