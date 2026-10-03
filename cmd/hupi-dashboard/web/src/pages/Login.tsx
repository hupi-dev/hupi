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
//     it server-side and redirects back here with the same {token} shape
//     delivered via a one-time query param this page reads on mount — see
//     docs/DASHBOARD.md's login section for the exact handoff.
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
    <div className="flex h-full items-center justify-center">
      <Card className="w-full max-w-sm">
        <h1 className="mb-4 text-lg font-semibold">Sign in to hupi-dashboard</h1>
        <form onSubmit={handleSubmit} className="space-y-3">
          <input
            className="w-full rounded border border-slate-700 bg-slate-800 px-3 py-2 text-sm"
            placeholder="Username"
            value={username}
            onChange={(e) => setUsername(e.target.value)}
            autoComplete="username"
          />
          <input
            className="w-full rounded border border-slate-700 bg-slate-800 px-3 py-2 text-sm"
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
            className="w-full rounded bg-accent px-3 py-2 text-sm font-medium text-white hover:bg-accent-hover disabled:opacity-50"
          >
            Sign in
          </button>
        </form>
        <div className="mt-4 border-t border-slate-800 pt-4">
          <a
            href="/auth/login/oidc/start"
            className="block w-full rounded border border-slate-700 px-3 py-2 text-center text-sm hover:bg-slate-800"
          >
            Sign in with SSO
          </a>
        </div>
      </Card>
    </div>
  );
}
