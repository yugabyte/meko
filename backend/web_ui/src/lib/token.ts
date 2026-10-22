// The client token the UI sends. meko ui opens the page with a one-time #code=, which is swapped
// here for the token; the token is then kept in this browser, so a plain http://127.0.0.1:7778/
// works afterwards.

const KEY = "meko.token";

let memoryToken = "";

function keep(token: string): void {
  memoryToken = token;
  try {
    localStorage.setItem(KEY, token);
  } catch {
    // Storage off: the token lives only for this page load.
  }
}

// Redeems the link's code, if any, and drops it from the address bar. A spent or expired code
// leaves whatever token the browser already had.
export async function adoptCodeFromURL(): Promise<void> {
  const m = /(?:^#|&)code=([^&]+)/.exec(window.location.hash);
  if (!m) return;
  history.replaceState(null, "", window.location.pathname + window.location.search);
  try {
    const res = await fetch("/api/v1/ui-code/redeem", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ code: decodeURIComponent(m[1]) }),
    });
    const env = await res.json();
    if (res.ok && typeof env?.data?.token === "string") keep(env.data.token);
  } catch {
    // Unreachable server: the app shows its own error.
  }
}

export function getToken(): string {
  try {
    return localStorage.getItem(KEY) ?? memoryToken;
  } catch {
    return memoryToken;
  }
}

export function clearToken(): void {
  memoryToken = "";
  try {
    localStorage.removeItem(KEY);
  } catch {
    // nothing kept
  }
}
