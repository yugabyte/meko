// cloud_admin password rules for an AMP read_write endpoint: generation and
// validation matching the controller (8-128 printable ASCII; it keeps only the
// SCRAM verifier, so the plaintext is caller-supplied and unrecoverable).
// Shared by the create-cluster wizard and any add-read_write-compute flow.

/** The fixed Postgres superuser AMP creates — clients connect as this role. The
 *  owner sets its password, not its name, so the username field is display-only. */
export const CLOUD_ADMIN_USER = "cloud_admin";

// Matches the controller's CreateEndpointRequest.password bounds (and Meko
// api_server's validator): printable ASCII U+0020-U+007E, 8-128 characters.
export const AMP_PASSWORD_MIN = 8;
export const AMP_PASSWORD_MAX = 128;

// A 64-char URL-safe alphabet: every character is printable ASCII and
// shell/JSON/connection-string safe, and 64 = 2^6 lets us map a random byte with
// `& 63` without modulo bias.
const GEN_ALPHABET = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_";
const GEN_LENGTH = 24;

/** A strong, unbiased cloud_admin password within the controller's bounds, from
 *  crypto randomness. Shown once at create time and never recoverable after. */
export function generateAmpPassword(length: number = GEN_LENGTH): string {
  const bytes = new Uint8Array(length);
  crypto.getRandomValues(bytes);
  let out = "";
  for (let i = 0; i < length; i++) out += GEN_ALPHABET[bytes[i] & 63];
  return out;
}

/** null when the password is valid, else a user-facing reason. Never trims — a
 *  space (U+0020) is a valid character and trimming would change the secret. */
export function validateAmpPassword(pw: string): string | null {
  if (pw.length < AMP_PASSWORD_MIN || pw.length > AMP_PASSWORD_MAX) {
    return `Password must be ${AMP_PASSWORD_MIN}-${AMP_PASSWORD_MAX} characters.`;
  }
  for (const ch of pw) {
    const c = ch.codePointAt(0)!;
    if (c < 0x20 || c > 0x7e) return "Password must use only printable ASCII characters.";
  }
  return null;
}
