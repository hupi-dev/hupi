import { FormEvent, useState } from "react";
import { Location, useLocation, useNavigate } from "react-router-dom";
import { ApiError } from "../lib/api";
import { useAuth } from "../lib/auth";
import { Button } from "../components/Button";
import { Field, inputClass } from "../components/Form";

interface LocationState {
  from?: Location;
}

export function Login() {
  const { login } = useAuth();
  const navigate = useNavigate();
  const location = useLocation();
  const [name, setName] = useState("");
  const [token, setToken] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const state = location.state as LocationState | null;
  const from = state?.from?.pathname ?? "/";

  async function onSubmit(e: FormEvent) {
    e.preventDefault();
    setError(null);
    setBusy(true);
    try {
      await login(name.trim(), token);
      navigate(from, { replace: true });
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) {
        setError("Invalid operator name or token.");
      } else {
        setError(err instanceof Error ? err.message : "Login failed.");
      }
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="flex min-h-screen items-center justify-center bg-slate-950 px-4">
      <form onSubmit={onSubmit} className="w-full max-w-sm rounded-lg border border-slate-800 bg-slate-900/60 p-6 shadow-xl">
        <h1 className="mb-1 text-xl font-semibold text-slate-100">hupi-admin-ui</h1>
        <p className="mb-6 text-sm text-slate-400">Sign in with your operator credential.</p>

        <div className="space-y-3">
          <Field label="Operator name (optional)">
            <input
              className={inputClass}
              value={name}
              onChange={(e) => setName(e.target.value)}
              autoFocus
              placeholder="alice"
              autoComplete="username"
            />
          </Field>

          <Field label="Token">
            <input
              type="password"
              className={inputClass}
              value={token}
              onChange={(e) => setToken(e.target.value)}
              placeholder="raw operator token"
              autoComplete="current-password"
            />
          </Field>
        </div>

        {error && <p className="mt-4 text-sm text-red-400">{error}</p>}

        <Button type="submit" className="mt-5 w-full" disabled={busy || !token}>
          {busy ? "Signing in…" : "Sign in"}
        </Button>
      </form>
    </div>
  );
}
