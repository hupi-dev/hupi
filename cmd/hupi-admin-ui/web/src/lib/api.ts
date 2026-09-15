import { clearAuth, loadAuth } from "./session";

// --- wire types ------------------------------------------------------------
// These mirror cmd/hupi-admin-ui/handlers.go's response shapes exactly:
// Go's default JSON field casing (PascalCase, no json tags) everywhere
// except the two hand-written bundle responses and the handful of
// lowercase-tagged action responses (raw_key, raw_token, status, error),
// which already use explicit lowercase tags in the Go source.

export interface User {
  ID: string;
  Email: string;
  CreatedAt: string;
}

export interface Team {
  ID: string;
  Name: string;
  CreatedAt: string;
}

export interface Membership {
  UserID: string;
  Role: string;
}

export interface APIKey {
  KeyHash: string;
  CreatedAt: string;
  RevokedAt: string | null;
}

export interface Operator {
  Name: string;
  CreatedAt: string;
  RevokedAt: string | null;
}

export interface UserBundle {
  user: User;
  teams: Team[];
  keys: APIKey[];
}

export interface TeamBundle {
  team: Team;
  members: Membership[];
}

export interface AuditEntry {
  TS: string;
  EventType: string;
  Actor: string;
  ActingScopeKind: string;
  ActingScopeOwner: string;
  WorkspaceScopeKind: string;
  WorkspaceScopeOwner: string;
  TargetRef: string;
  Detail: string;
}

export interface AuditQueryParams {
  scope_kind?: string;
  scope_owner?: string;
  actor?: string;
  event_type?: string;
  since?: string;
  until?: string;
  limit?: number;
}

// --- error type --------------------------------------------------------

export class ApiError extends Error {
  status: number;
  constructor(status: number, message: string) {
    super(message);
    this.name = "ApiError";
    this.status = status;
  }
}

// --- transport -----------------------------------------------------------

const BASE = "/api";

function orEmpty<T>(v: T[] | null | undefined): T[] {
  return v ?? [];
}

async function parseBody(res: Response): Promise<unknown> {
  const text = await res.text();
  if (!text) return null;
  try {
    return JSON.parse(text);
  } catch {
    return null;
  }
}

function errorMessage(body: unknown, status: number): string {
  if (body && typeof body === "object" && "error" in body && typeof (body as { error: unknown }).error === "string") {
    return (body as { error: string }).error;
  }
  return `request failed with status ${status}`;
}

/**
 * verifyCredentials performs a one-off GET /api/whoami with an explicit
 * Authorization header, bypassing sessionStorage entirely — used only by
 * the login screen to validate a name/token pair before anything is
 * persisted.
 */
export async function verifyCredentials(basicAuthValue: string): Promise<{ name: string }> {
  const res = await fetch(`${BASE}/whoami`, {
    headers: { Authorization: `Basic ${basicAuthValue}`, Accept: "application/json" },
  });
  const body = await parseBody(res);
  if (!res.ok) {
    throw new ApiError(res.status, errorMessage(body, res.status));
  }
  return body as { name: string };
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const auth = loadAuth();
  const headers = new Headers(init?.headers);
  headers.set("Accept", "application/json");
  if (init?.body !== undefined) headers.set("Content-Type", "application/json");
  if (auth) headers.set("Authorization", `Basic ${auth.basicAuthValue}`);

  const res = await fetch(BASE + path, { ...init, headers });

  if (res.status === 401) {
    // Handles token revocation mid-session: whatever credential we had
    // stopped working, so drop it and send the operator back to the
    // login screen rather than showing a half-broken dashboard.
    clearAuth();
    if (window.location.pathname !== "/login") {
      window.location.assign("/login");
    }
    throw new ApiError(401, "unauthorized");
  }

  const body = await parseBody(res);
  if (!res.ok) {
    throw new ApiError(res.status, errorMessage(body, res.status));
  }
  return body as T;
}

// --- API surface -----------------------------------------------------------

export const api = {
  whoami: () => request<{ name: string }>("/whoami"),

  listUsers: () => request<User[] | null>("/users").then(orEmpty),
  createUser: (id: string, email: string) =>
    request<User>("/users", { method: "POST", body: JSON.stringify({ id, email }) }),
  getUser: (id: string) =>
    request<UserBundle>(`/users/${encodeURIComponent(id)}`).then((b) => ({
      ...b,
      teams: orEmpty(b.teams),
      keys: orEmpty(b.keys),
    })),
  issueKey: (userId: string) =>
    request<{ raw_key: string }>(`/users/${encodeURIComponent(userId)}/keys`, { method: "POST" }),
  revokeKey: (keyHash: string, userId: string) =>
    request<{ status: string }>("/keys/revoke", {
      method: "POST",
      body: JSON.stringify({ key_hash: keyHash, user_id: userId }),
    }),

  listTeams: () => request<Team[] | null>("/teams").then(orEmpty),
  createTeam: (id: string, name: string) =>
    request<Team>("/teams", { method: "POST", body: JSON.stringify({ id, name }) }),
  getTeam: (id: string) =>
    request<TeamBundle>(`/teams/${encodeURIComponent(id)}`).then((b) => ({
      ...b,
      members: orEmpty(b.members),
    })),
  addMember: (teamId: string, userId: string, role: string) =>
    request<{ status: string }>(`/teams/${encodeURIComponent(teamId)}/members`, {
      method: "POST",
      body: JSON.stringify({ user_id: userId, role }),
    }),

  listOperators: () => request<Operator[] | null>("/operators").then(orEmpty),
  createOperator: (name: string) =>
    request<{ raw_token: string }>("/operators", { method: "POST", body: JSON.stringify({ name }) }),
  revokeOperator: (name: string) =>
    request<{ status: string }>("/operators/revoke", { method: "POST", body: JSON.stringify({ name }) }),

  queryAudit: (params: AuditQueryParams) => {
    const q = new URLSearchParams();
    for (const [k, v] of Object.entries(params)) {
      if (v !== undefined && v !== null && v !== "") q.set(k, String(v));
    }
    const qs = q.toString();
    return request<AuditEntry[] | null>(`/audit${qs ? `?${qs}` : ""}`).then(orEmpty);
  },
};
