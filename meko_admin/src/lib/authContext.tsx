import {
  createContext,
  useContext,
  useState,
  useEffect,
  useCallback,
  type ReactNode,
} from "react";
import axios from "axios";

export type AuthUser = { sub: string; email: string };

type LoginResult =
  | { type: "success" }
  | { type: "new_password_required"; session: string; email: string };

type AuthContextValue = {
  user: AuthUser | null;
  isLoading: boolean;
  login: (email: string, password: string) => Promise<LoginResult>;
  completeNewPassword: (email: string, newPassword: string, session: string, givenName: string, familyName: string) => Promise<void>;
  logout: () => void;
};

const AuthContext = createContext<AuthContextValue | null>(null);

const TOKEN_KEY = "admin_access_token";

export const authHttp = axios.create({ baseURL: "/api/admin/auth" });
export const adminHttp = axios.create({ baseURL: "/api/admin" });

function setToken(token: string) {
  sessionStorage.setItem(TOKEN_KEY, token);
  adminHttp.defaults.headers.common["Authorization"] = `Bearer ${token}`;
}

function clearToken() {
  sessionStorage.removeItem(TOKEN_KEY);
  delete adminHttp.defaults.headers.common["Authorization"];
}

export function AuthProvider({ children }: { children: ReactNode }) {
  const [user, setUser] = useState<AuthUser | null>(null);
  const [isLoading, setIsLoading] = useState(true);

  useEffect(() => {
    const token = sessionStorage.getItem(TOKEN_KEY);
    if (!token) {
      setIsLoading(false);
      return;
    }
    adminHttp.defaults.headers.common["Authorization"] = `Bearer ${token}`;
    authHttp
      .get<AuthUser>("/me", { headers: { Authorization: `Bearer ${token}` } })
      .then(({ data }) => setUser(data))
      .catch(() => clearToken())
      .finally(() => setIsLoading(false));
  }, []);

  const login = useCallback(async (email: string, password: string): Promise<LoginResult> => {
    const { data } = await authHttp.post<
      | { accessToken: string; expiresIn: number }
      | { challenge: "NEW_PASSWORD_REQUIRED"; session: string; email: string }
    >("/login", { email, password });

    if ("challenge" in data) {
      return { type: "new_password_required", session: data.session, email: data.email };
    }

    setToken(data.accessToken);
    const { data: me } = await authHttp.get<AuthUser>("/me", {
      headers: { Authorization: `Bearer ${data.accessToken}` },
    });
    setUser(me);
    return { type: "success" };
  }, []);

  const completeNewPassword = useCallback(
    async (email: string, newPassword: string, session: string, givenName: string, familyName: string) => {
      const { data } = await authHttp.post<{ accessToken: string; expiresIn: number }>(
        "/new-password",
        { email, newPassword, session, givenName, familyName },
      );
      setToken(data.accessToken);
      const { data: me } = await authHttp.get<AuthUser>("/me", {
        headers: { Authorization: `Bearer ${data.accessToken}` },
      });
      setUser(me);
    },
    [],
  );

  const logout = useCallback(() => {
    clearToken();
    setUser(null);
  }, []);

  return (
    <AuthContext.Provider value={{ user, isLoading, login, completeNewPassword, logout }}>
      {children}
    </AuthContext.Provider>
  );
}

export function useAuth() {
  const ctx = useContext(AuthContext);
  if (!ctx) throw new Error("useAuth must be used inside AuthProvider");
  return ctx;
}
