import { FormEvent, useState } from "react";
import { saveSessionToken } from "../lib/session";
import { Card } from "../components/Card";

// Only ever rendered on Tier 3 (see App.tsx) — hupi-t3's
// cmd/hupi-dashboard/team.go overlay is what actually makes
// /auth/login/password and /auth/login/oidc/start real routes; on this
// OSS build /api/whoami never 401s, so this component never mounts.
//
// Wire contract this expects from the Tier 3 overlay:
//   POST /auth/login/password {username, password} -> 200 {token: string} | 4xx {error: string}
//   GET  /auth/login/oidc/start -> redirects the browser into the IdP's
//     Authorization Code + PKCE flow; /auth/login/oidc/callback completes
//     it server-side and redirects back to "/" with the session token in
//     a one-time ?dashboard_token=... query param, which App.tsx's
//     consumeOidcCallbackToken reads, saves, and strips from the URL —
//     see docs/DASHBOARD.md's login section for the full handoff.
export function Login({ onLoggedIn }: { onLoggedIn: () => void }) {
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);

  const handleSubmit = async (e: FormEvent) => {
    e.preventDefault();
    setSubmitting(true);
    setError(null);
    try {
      const res = await fetch("/auth/login/password", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ username, password }),
      });
      const body = await res.json().catch(() => null);
      if (!res.ok) {
        throw new Error((body && body.error) || "sign-in failed");
      }
      saveSessionToken(body.token as string);
      onLoggedIn();
    } catch (err) {
      setError(err instanceof Error ? err.message : "sign-in failed");
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <div className="relative flex h-full min-h-screen items-center justify-center overflow-hidden">
      <div
        aria-hidden
        className="pointer-events-none absolute left-1/2 top-1/3 h-[28rem] w-[28rem] -translate-x-1/2 -translate-y-1/2 rounded-full bg-ember-600/20 blur-[120px]"
      />
      <div
        aria-hidden
        className="bg-grid pointer-events-none absolute inset-0 [mask-image:radial-gradient(ellipse_60%_60%_at_50%_40%,black,transparent)]"
      />
      <Card className="relative w-full max-w-sm">
        <p className="font-mono-tight mb-1 text-xs uppercase tracking-widest text-ember-500">hupi-dashboard</p>
        <h1 className="mb-4 text-lg font-bold text-fog-100">Sign in to your memory</h1>
        <form onSubmit={handleSubmit} className="space-y-3">
          <input
            className="w-full rounded-lg border border-white/15 bg-navy-950 px-3 py-2 text-sm text-fog-100 placeholder:text-fog-700 focus:border-ember-500 focus:outline-none"
            placeholder="Username"
            value={username}
            onChange={(e) => setUsername(e.target.value)}
            autoComplete="username"
          />
          <input
            className="w-full rounded-lg border border-white/15 bg-navy-950 px-3 py-2 text-sm text-fog-100 placeholder:text-fog-700 focus:border-ember-500 focus:outline-none"
            placeholder="Password"
            type="password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            autoComplete="current-password"
          />
          {error && <p className="text-sm text-red-400">{error}</p>}
          <button
            type="submit"
            disabled={submitting}
            className="w-full rounded-lg bg-ember-500 px-3 py-2 text-sm font-semibold text-ink shadow-lg shadow-ember-600/20 transition-all hover:bg-ember-400 hover:shadow-ember-500/30 disabled:opacity-50"
          >
            Sign in
          </button>
        </form>
        <div className="mt-4 border-t border-white/10 pt-4">
          <a
            href="/auth/login/oidc/start"
            className="block w-full rounded-lg border border-white/15 px-3 py-2 text-center text-sm text-fog-100 transition-colors hover:border-white/30 hover:bg-white/5"
          >
            Sign in with SSO
          </a>
        </div>
      </Card>
    </div>
  );
}
