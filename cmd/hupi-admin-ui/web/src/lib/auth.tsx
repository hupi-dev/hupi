import { createContext, ReactNode, useCallback, useContext, useState } from "react";
import { Navigate, useLocation } from "react-router-dom";
import { verifyCredentials } from "./api";
import { clearAuth, loadAuth, saveAuth, StoredAuth } from "./session";

interface AuthContextValue {
  operatorName: string | null;
  authHeader: string | null;
  login: (name: string, token: string) => Promise<void>;
  logout: () => void;
}

const AuthContext = createContext<AuthContextValue | null>(null);

export function AuthProvider({ children }: { children: ReactNode }) {
  const [auth, setAuth] = useState<StoredAuth | null>(() => loadAuth());

  const login = useCallback(async (name: string, token: string) => {
    const basicAuthValue = btoa(`${name}:${token}`);
    // Verify against the real API before persisting anything — on a 401
    // this throws and nothing is stored (per spec: "on 401, show an
    // inline error, don't store anything").
    const result = await verifyCredentials(basicAuthValue);
    const stored: StoredAuth = { name: result.name || name, basicAuthValue };
    saveAuth(stored);
    setAuth(stored);
  }, []);

  const logout = useCallback(() => {
    clearAuth();
    setAuth(null);
  }, []);

  const value: AuthContextValue = {
    operatorName: auth?.name ?? null,
    authHeader: auth ? `Basic ${auth.basicAuthValue}` : null,
    login,
    logout,
  };

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

export function useAuth(): AuthContextValue {
  const ctx = useContext(AuthContext);
  if (!ctx) throw new Error("useAuth must be used inside an AuthProvider");
  return ctx;
}

// RequireAuth is optimistic UX only — it just keeps a logged-out browser
// from flashing a broken dashboard. The real enforcement is server-side
// (requireOperatorAuth in cmd/hupi-admin-ui/main.go); every API call still
// needs a valid Basic Auth credential regardless of what this does.
export function RequireAuth({ children }: { children: JSX.Element }) {
  const { operatorName } = useAuth();
  const location = useLocation();

  if (!operatorName) {
    return <Navigate to="/login" replace state={{ from: location }} />;
  }
  return children;
}
