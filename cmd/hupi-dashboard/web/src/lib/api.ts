import { clearSessionToken, loadSessionToken } from "./session";

// --- wire types ------------------------------------------------------------
// Mirror cmd/hupi-dashboard/{queries,handlers}.go's response shapes
// exactly (lowercase json tags set explicitly in the Go source).

export interface ConversationVolumePoint {
  day: string;
  count: number;
}

export interface ThemeWordCloudEntry {
  entity_id: string;
  name: string;
  kind: string;
  mentions: number;
}

export interface EntityRelationship {
  subject_id: string;
  predicate: string;
  object_id: string;
  valid_from?: string;
  valid_until?: string;
  created_at: string;
}

export interface MemoryHealth {
  episode_count: number;
  summary_count: number;
  corpus_size_updated_at?: string;
  last_summary_at?: string;
  corrections_count: number;
}

export interface KeywordSearchGovernance {
  tier: "full" | "narrowed" | "disabled";
  total_corpus_size: number;
  narrow_threshold: number;
  disable_threshold: number;
}

export interface SecurityEvent {
  ts: string;
  event_type: string;
  actor: string;
}

export interface KeyRotationStatus {
  from_version: number;
  to_version: number;
  status: string;
  started_at: string;
  completed_at?: string;
}

// recent_events is typed non-nullable here even though the wire response
// can send null for "no events" — api.securityPosture's own .then()
// below normalizes that to [] before this type is ever seen by a
// component, same orEmpty pattern every other list field in this file
// uses.
export interface SecurityPosture {
  key_rotation?: KeyRotationStatus;
  recent_events: SecurityEvent[];
}

export interface Whoami {
  scope_kind: string;
  scope_owner: string;
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

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const token = loadSessionToken();
  const headers = new Headers(init?.headers);
  headers.set("Accept", "application/json");
  if (token) headers.set("Authorization", `Bearer ${token}`);

  const res = await fetch(BASE + path, { ...init, headers });

  if (res.status === 401) {
    // On Tier 1/2 this never fires (no auth at all). On Tier 3, the
    // session token stopped working (expired/revoked) — drop it and
    // let App's whoami check redirect to the login screen rather than
    // showing a half-broken dashboard.
    clearSessionToken();
    throw new ApiError(401, "unauthorized");
  }

  const body = await parseBody(res);
  if (!res.ok) {
    throw new ApiError(res.status, errorMessage(body, res.status));
  }
  return body as T;
}

export const api = {
  whoami: () => request<Whoami>("/whoami"),
  conversationVolume: (days = 90) => request<ConversationVolumePoint[] | null>(`/conversation-volume?days=${days}`).then(orEmpty),
  themeWordCloud: (limit = 50) => request<ThemeWordCloudEntry[] | null>(`/theme-word-cloud?limit=${limit}`).then(orEmpty),
  entityRelationships: (limit = 200) => request<EntityRelationship[] | null>(`/entity-relationships?limit=${limit}`).then(orEmpty),
  memoryHealth: () => request<MemoryHealth>("/memory-health"),
  keywordSearchGovernance: () => request<KeywordSearchGovernance>("/keyword-search-governance"),
  securityPosture: (limit = 50) =>
    request<Omit<SecurityPosture, "recent_events"> & { recent_events: SecurityEvent[] | null }>(
      `/security-posture?limit=${limit}`,
    ).then((p) => ({ ...p, recent_events: orEmpty(p.recent_events) })),
};
