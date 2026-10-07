import { clearSessionToken, loadSessionToken, loadWorkspace } from "./session";

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

export interface ForgottenEpisode {
  id: string;
  importance: number;
  ts: string;
}

export interface StaleEntity {
  id: string;
  name: string;
  kind: string;
  last_updated: string;
}

// episodes/entities are typed non-nullable for the same reason
// SecurityPosture.recent_events is below — api.forgottenButImportant's
// .then() normalizes both before a component ever sees this type.
export interface ForgottenButImportant {
  episodes: ForgottenEpisode[];
  entities: StaleEntity[];
}

// TermFrequency is Phase 2a's (decrypt-on-view, opt-in) local
// keyword-frequency output — see cmd/hupi-dashboard/content_analysis.go.
export interface TermFrequency {
  term: string;
  count: number;
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

// Workspace is one team the signed-in user can switch into — Tier 3
// only; GET /api/workspaces doesn't exist at all on the OSS build (no
// hupi-t3 overlay to mount it), so api.listWorkspaces treats any error
// as "not available" rather than surfacing it.
export interface Workspace {
  id: string;
  name: string;
}

// --- memory map ------------------------------------------------------------
// Mirror cmd/hupi-dashboard/memory_map.go's response shapes exactly.
// Two endpoints, two response shapes — the cheap always-on graph
// (MemoryMapGraph) and the decrypt-gated topics overlay
// (MemoryMapTopicsResponse) — merged into one cytoscape element list by
// lib/memoryMapGraph.ts, not here.

export interface MemoryMapEntityPayload {
  entity_id: string;
  kind: string;
}

export interface MemoryMapConversationPayload {
  episode_id: string;
  ts: string;
  importance?: number;
}

export interface MemoryMapTopicPayload {
  term: string;
  count: number;
}

export interface MemoryMapNode {
  id: string;
  type: "entity" | "conversation" | "topic";
  label: string;
  entity?: MemoryMapEntityPayload;
  conversation?: MemoryMapConversationPayload;
  topic?: MemoryMapTopicPayload;
}

export interface MemoryMapRelationshipPayload {
  predicate: string;
  valid_from?: string;
  valid_until?: string;
}

export interface MemoryMapEdge {
  id: string;
  source: string;
  target: string;
  type: "relationship" | "mentions" | "tagged_with";
  relationship?: MemoryMapRelationshipPayload;
}

export interface MemoryMapGraph {
  range: { from: string; to: string };
  nodes: MemoryMapNode[];
  edges: MemoryMapEdge[];
  total_node_count: number;
  truncated: boolean;
}

export interface MemoryMapConversationTopicsEntry {
  episode_id: string;
  excerpt: string;
  topics: TermFrequency[];
}

export interface MemoryMapTopicsResponse {
  enabled: boolean;
  truncated?: boolean;
  conversations?: MemoryMapConversationTopicsEntry[];
  topics?: MemoryMapNode[];
  edges?: MemoryMapEdge[];
}

export interface MemoryMapDateFilter {
  days?: number;
  from?: string;
  to?: string;
}

function memoryMapQueryString(filter: MemoryMapDateFilter, extra?: Record<string, number>): string {
  const params = new URLSearchParams();
  if (filter.from && filter.to) {
    params.set("from", filter.from);
    params.set("to", filter.to);
  } else {
    params.set("days", String(filter.days ?? 30));
  }
  if (extra) {
    for (const [k, v] of Object.entries(extra)) params.set(k, String(v));
  }
  return params.toString();
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
  const workspace = loadWorkspace();
  const headers = new Headers(init?.headers);
  headers.set("Accept", "application/json");
  if (token) headers.set("Authorization", `Bearer ${token}`);
  // Tier 3 only — hupi-t3's resolveDashboardSession reads this to route
  // to a shared team scope instead of the signed-in user's own private
  // one; ignored entirely on Tier 1/2 (no session, so no header is sent).
  if (workspace) headers.set("X-Hupi-Workspace", workspace);

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
  forgottenButImportant: () =>
    request<{ episodes: ForgottenEpisode[] | null; entities: StaleEntity[] | null }>("/forgotten-but-important").then((r) => ({
      episodes: orEmpty(r.episodes),
      entities: orEmpty(r.entities),
    })),
  keywordSearchGovernance: () => request<KeywordSearchGovernance>("/keyword-search-governance"),
  securityPosture: (limit = 50) =>
    request<Omit<SecurityPosture, "recent_events"> & { recent_events: SecurityEvent[] | null }>(
      `/security-posture?limit=${limit}`,
    ).then((p) => ({ ...p, recent_events: orEmpty(p.recent_events) })),
  // Goes through request() (not a plain <a href>) specifically so the
  // Bearer session token (Tier 3) is attached — a bare link navigation
  // can't set a custom header, so Dashboard.tsx turns this JSON back
  // into a Blob and triggers the actual file download itself.
  exportMemory: () => request<unknown>("/export"),
  contentThemes: (days = 30) =>
    request<{ enabled: boolean; terms?: TermFrequency[] }>(`/content-themes?days=${days}`),
  contentThemesNarrative: (days = 30) =>
    request<{ enabled: boolean; narrative?: string }>(`/content-themes/narrative?days=${days}`),
  listWorkspaces: () => request<Workspace[] | null>("/workspaces").then(orEmpty).catch(() => [] as Workspace[]),
  memoryMap: (filter: MemoryMapDateFilter, maxConversations?: number, maxRelationships?: number) =>
    request<MemoryMapGraph>(
      `/memory-map?${memoryMapQueryString(filter, {
        ...(maxConversations ? { max_conversations: maxConversations } : {}),
        ...(maxRelationships ? { max_relationships: maxRelationships } : {}),
      })}`,
    ),
  memoryMapTopics: (filter: MemoryMapDateFilter, maxConversations?: number) =>
    request<MemoryMapTopicsResponse>(
      `/memory-map/topics?${memoryMapQueryString(filter, maxConversations ? { max_conversations: maxConversations } : undefined)}`,
    ),
};
