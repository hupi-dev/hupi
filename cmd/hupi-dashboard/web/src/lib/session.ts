// Session storage for the dashboard's opaque bearer session token
// (Tier 3 only — hupi-t3's login routes mint it). Deliberately
// sessionStorage, not localStorage or a cookie — cleared when the tab
// closes, and never auto-resent by the browser cross-site, which is
// exactly why requireDashboardSession (cmd/hupi-dashboard/auth.go) needs
// no CSRF mitigation. Same pattern cmd/hupi-admin-ui/web/src/lib/session.ts
// already uses for its own (Basic Auth) credential.
const STORAGE_KEY = "hupi_dashboard_session_token";

export function loadSessionToken(): string | null {
  return sessionStorage.getItem(STORAGE_KEY);
}

export function saveSessionToken(token: string): void {
  sessionStorage.setItem(STORAGE_KEY, token);
}

export function clearSessionToken(): void {
  sessionStorage.removeItem(STORAGE_KEY);
}
