// Session storage for the operator's Basic Auth credential. Deliberately
// sessionStorage, not localStorage — cleared when the tab closes, matching
// the security posture docs/ADMIN_UI.md describes for operator tokens
// ("treat every operator token with the same care as an API key").
export interface StoredAuth {
  name: string;
  // base64("<name>:<raw token>") — the exact value that goes after
  // "Basic " in the Authorization header.
  basicAuthValue: string;
}

const STORAGE_KEY = "hupi_admin_ui_auth";

export function loadAuth(): StoredAuth | null {
  const raw = sessionStorage.getItem(STORAGE_KEY);
  if (!raw) return null;
  try {
    const parsed = JSON.parse(raw);
    if (parsed && typeof parsed.name === "string" && typeof parsed.basicAuthValue === "string") {
      return parsed as StoredAuth;
    }
  } catch {
    // corrupt value — treat as logged out
  }
  return null;
}

export function saveAuth(auth: StoredAuth): void {
  sessionStorage.setItem(STORAGE_KEY, JSON.stringify(auth));
}

export function clearAuth(): void {
  sessionStorage.removeItem(STORAGE_KEY);
}
