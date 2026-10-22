// The client token the UI sends: taken from the #token= fragment meko ui opens it with, then kept
// in this browser so a plain http://127.0.0.1:7778/ works afterwards.

const KEY = "meko.token";

export function adoptTokenFromURL(): void {
  const m = /(?:^#|&)token=([^&]+)/.exec(window.location.hash);
  if (!m) return;
  try {
    localStorage.setItem(KEY, decodeURIComponent(m[1]));
  } catch {
    // Storage off: the token lives only for this page load.
    memoryToken = decodeURIComponent(m[1]);
  }
  history.replaceState(null, "", window.location.pathname + window.location.search);
}

let memoryToken = "";

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
