import { useEffect, useState } from "react";
import { api, ApiError, Whoami } from "./lib/api";
import { Dashboard } from "./pages/Dashboard";
import { Login } from "./pages/Login";

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
    return <div className="flex h-full items-center justify-center text-slate-400">Loading…</div>;
  }
  if (needsLogin || !whoami) {
    return <Login onLoggedIn={checkSession} />;
  }
  return <Dashboard whoami={whoami} />;
}
