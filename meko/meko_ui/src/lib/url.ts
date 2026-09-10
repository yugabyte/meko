/** Origin only, not the full URL — callers use this to log/display a host
 *  without leaking a query string that may carry credentials. */
export function safeOrigin(url: string): string {
  try {
    return new URL(url).origin;
  } catch {
    return url;
  }
}
