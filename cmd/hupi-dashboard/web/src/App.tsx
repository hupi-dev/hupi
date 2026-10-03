import { useEffect, useState } from "react";
import { api, ApiError, Whoami } from "./lib/api";
import { saveSessionToken } from "./lib/session";
import { Dashboard } from "./pages/Dashboard";
import { Login } from "./pages/Login";

// Picks up the one-time ?dashboard_token=... query param hupi-t3's
// /auth/login/oidc/callback redirects back here with (see Login.tsx's
// own doc comment for the full OIDC handoff) — saves it the same way a
// password login's response does, then strips it from the URL so it
// never ends up in browser history or gets re-read on a later refresh.
function consumeOidcCallbackToken(): boolean {
  const params = new URLSearchParams(window.location.search);
  const token = params.get("dashboard_token");
  if (!token) return false;
  saveSessionToken(token);
  params.delete("dashboard_token");
  const query = params.toString();
  window.history.replaceState({}, "", window.location.pathname + (query ? `?${query}` : ""));
  return true;
}

// App's whole job is one check: can /api/whoami be reached without
// logging in? On Tier 1/2 (this OSS build, resolveDashboardSession is
// nil) it always can — every request already resolves to
// identity.DefaultScope server-side, so Login never renders there at
// all. On Tier 3, a 401 here means hupi-t3's login routes are mounted
// and a real session is required first.
export function App() {
  const [whoami, setWhoami] = useState<Whoami | null>(null);
  const [needsLogin, setNeedsLogin] = useState(false);
  const [loading, setLoading] = useState(true);

  const checkSession = () => {
    setLoading(true);
    consumeOidcCallbackToken();
    api
      .whoami()
      .then((w) => {
        setWhoami(w);
        setNeedsLogin(false);
      })
      .catch((err) => {
        if (err instanceof ApiError && err.status === 401) {
          setNeedsLogin(true);
        }
      })
      .finally(() => setLoading(false));
  };

  useEffect(checkSession, []);

  if (loading) {
    return <div className="flex h-full min-h-screen items-center justify-center bg-ink text-fog-500">Loading…</div>;
  }
  if (needsLogin || !whoami) {
    return <Login onLoggedIn={checkSession} />;
  }
  return <Dashboard whoami={whoami} />;
}
