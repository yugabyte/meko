// Client-only auth context for AMP. There is NO backend yet — sign-in is
// gated to a single hard-coded admin credential, while sign up accepts
// any input. Both stash a fake user in localStorage and flip the app
// into its authenticated state. Swap the body of `signIn` / `signUp`
// for real API calls when a backend lands; the surface (`useAuth`)
// stays the same.

/** Temporary hard-coded login until a real backend exists: admin/admin. */
export const ADMIN_USERNAME = "admin";
export const ADMIN_PASSWORD = "admin";

import {
  createContext,
  useCallback,
  useContext,
  useMemo,
  useState,
  type ReactNode,
} from "react";

export type AmpUser = {
  email: string;
  name: string;
};

type AuthContextValue = {
  user: AmpUser | null;
  isAuthenticated: boolean;
  signIn: (email: string, password: string) => Promise<void>;
  signUp: (name: string, email: string, password: string) => Promise<void>;
  signOut: () => void;
};

/** localStorage key for the fake session. `amp:` prefix mirrors the
 *  `meko:` convention so persisted prefs stay namespaced. */
const AUTH_STORAGE_KEY = "amp:user";

const AuthContext = createContext<AuthContextValue | null>(null);

function readStoredUser(): AmpUser | null {
  if (typeof window === "undefined") return null;
  try {
    const raw = window.localStorage.getItem(AUTH_STORAGE_KEY);
    if (!raw) return null;
    const parsed = JSON.parse(raw) as Partial<AmpUser>;
    if (typeof parsed.email === "string" && typeof parsed.name === "string") {
      return { email: parsed.email, name: parsed.name };
    }
  } catch {
    // corrupt / unreadable — treat as logged out
  }
  return null;
}

function persistUser(user: AmpUser | null): void {
  try {
    if (user) window.localStorage.setItem(AUTH_STORAGE_KEY, JSON.stringify(user));
    else window.localStorage.removeItem(AUTH_STORAGE_KEY);
  } catch {
    // storage unavailable — in-memory state still works for this session
  }
}

/** Derive a display name from an email local-part when none is given. */
function nameFromEmail(email: string): string {
  const local = email.split("@")[0] ?? "";
  return local ? local.charAt(0).toUpperCase() + local.slice(1) : "there";
}

export function AuthProvider({ children }: { children: ReactNode }) {
  const [user, setUser] = useState<AmpUser | null>(readStoredUser);

  const signIn = useCallback(async (username: string, password: string) => {
    // Gate to the temporary admin credential until a backend lands.
    if (username.trim() !== ADMIN_USERNAME || password !== ADMIN_PASSWORD) {
      throw new Error("Invalid credentials");
    }
    const next: AmpUser = { email: ADMIN_USERNAME, name: "Admin" };
    persistUser(next);
    setUser(next);
  }, []);

  const signUp = useCallback(
    async (name: string, email: string, _password: string) => {
      const next: AmpUser = { email, name: name.trim() || nameFromEmail(email) };
      persistUser(next);
      setUser(next);
    },
    [],
  );

  const signOut = useCallback(() => {
    persistUser(null);
    setUser(null);
  }, []);

  const value = useMemo<AuthContextValue>(
    () => ({ user, isAuthenticated: user !== null, signIn, signUp, signOut }),
    [user, signIn, signUp, signOut],
  );

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

export function useAuth(): AuthContextValue {
  const ctx = useContext(AuthContext);
  if (!ctx) throw new Error("useAuth must be used within an <AuthProvider>");
  return ctx;
}
