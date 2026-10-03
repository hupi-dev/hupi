import { useEffect, useState } from "react";
import {
  api,
  ConversationVolumePoint,
  EntityRelationship,
  ForgottenButImportant as ForgottenButImportantData,
  KeywordSearchGovernance,
  MemoryHealth,
  SecurityPosture,
  TermFrequency,
  ThemeWordCloudEntry,
  Whoami,
  Workspace,
} from "../lib/api";
import { loadWorkspace, saveWorkspace } from "../lib/session";
import { Card, CardHeader, CardTitle } from "../components/Card";

function useLoaded<T>(load: () => Promise<T>): T | null {
  const [value, setValue] = useState<T | null>(null);
  useEffect(() => {
    let cancelled = false;
    load().then((v) => {
      if (!cancelled) setValue(v);
    });
    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);
  return value;
}

export function Dashboard({ whoami }: { whoami: Whoami }) {
  const volume = useLoaded<ConversationVolumePoint[]>(() => api.conversationVolume());
  const themes = useLoaded<ThemeWordCloudEntry[]>(() => api.themeWordCloud());
  const relationships = useLoaded<EntityRelationship[]>(() => api.entityRelationships());
  const health = useLoaded<MemoryHealth>(() => api.memoryHealth());
  const forgotten = useLoaded<ForgottenButImportantData>(() => api.forgottenButImportant());
  const governance = useLoaded<KeywordSearchGovernance>(() => api.keywordSearchGovernance());
  const security = useLoaded<SecurityPosture>(() => api.securityPosture());
  // Phase 2, opt-in and off by default server-side (docs/DASHBOARD.md) —
  // both come back as {enabled:false} rather than an error when their
  // env var isn't set, so these render nothing rather than a broken card.
  const contentThemes = useLoaded<{ enabled: boolean; terms?: TermFrequency[] }>(() => api.contentThemes());
  const narrative = useLoaded<{ enabled: boolean; narrative?: string }>(() => api.contentThemesNarrative());
  // Tier 3 only — [] on Tier 1/2 (no hupi-t3 overlay to mount
  // GET /api/workspaces at all), so the switcher below renders nothing.
  const workspaces = useLoaded<Workspace[]>(() => api.listWorkspaces());
  const [exporting, setExporting] = useState(false);

  const handleWorkspaceChange = (teamID: string) => {
    saveWorkspace(teamID);
    // Every panel's useLoaded effect only fetches once, on mount — a
    // full reload is the simplest way to make every single panel pick
    // up the new X-Hupi-Workspace header consistently, rather than
    // threading a workspace dependency through each of the eight
    // separate useLoaded calls above individually.
    window.location.reload();
  };

  const handleExport = async () => {
    setExporting(true);
    try {
      const data = await api.exportMemory();
      const blob = new Blob([JSON.stringify(data, null, 2)], { type: "application/json" });
      const url = URL.createObjectURL(blob);
      const a = document.createElement("a");
      a.href = url;
      a.download = `hupi-export-${whoami.scope_owner}.json`;
      a.click();
      URL.revokeObjectURL(url);
    } finally {
      setExporting(false);
    }
  };

  return (
    <div className="mx-auto max-w-5xl space-y-4 p-6">
      <header className="flex items-center justify-between">
        <h1 className="text-xl font-semibold">hupi-dashboard</h1>
        <div className="flex items-center gap-3">
          {workspaces && workspaces.length > 0 && (
            <select
              className="rounded border border-slate-700 bg-slate-900 px-2 py-1.5 text-sm"
              value={loadWorkspace() ?? ""}
              onChange={(e) => handleWorkspaceChange(e.target.value)}
            >
              <option value="">My own memory</option>
              {workspaces.map((w) => (
                <option key={w.id} value={w.id}>
                  {w.name}
                </option>
              ))}
            </select>
          )}
          <span className="text-sm text-slate-400">{whoami.scope_kind}:{whoami.scope_owner}</span>
          <button
            onClick={handleExport}
            disabled={exporting}
            className="rounded border border-slate-700 px-3 py-1.5 text-sm hover:bg-slate-800 disabled:opacity-50"
          >
            {exporting ? "Exporting…" : "Export my memory"}
          </button>
        </div>
      </header>

      <div className="grid grid-cols-1 gap-4 md:grid-cols-2">
        <Card>
          <CardHeader>
            <CardTitle>Conversation volume (last 90 days)</CardTitle>
          </CardHeader>
          {volume === null ? (
            <p className="text-sm text-slate-400">Loading…</p>
          ) : volume.length === 0 ? (
            <p className="text-sm text-slate-400">No interactions yet.</p>
          ) : (
            <ul className="space-y-1 text-sm">
              {volume.map((p) => (
                <li key={p.day} className="flex justify-between">
                  <span className="text-slate-400">{p.day}</span>
                  <span>{p.count}</span>
                </li>
              ))}
            </ul>
          )}
        </Card>

        <Card>
          <CardHeader>
            <CardTitle>Themes</CardTitle>
          </CardHeader>
          {themes === null ? (
            <p className="text-sm text-slate-400">Loading…</p>
          ) : themes.length === 0 ? (
            <p className="text-sm text-slate-400">Nothing consolidated yet.</p>
          ) : (
            <div className="flex flex-wrap gap-2">
              {themes.map((t) => (
                <span
                  key={t.entity_id}
                  className="rounded-full bg-slate-800 px-3 py-1 text-sm"
                  style={{ fontSize: `${Math.min(1.6, 0.8 + t.mentions / 10)}rem` }}
                  title={`${t.kind}, mentioned ${t.mentions}x`}
                >
                  {t.name}
                </span>
              ))}
            </div>
          )}
        </Card>

        {contentThemes?.enabled && (
          <Card>
            <CardHeader>
              <CardTitle>Content themes (decrypted on view)</CardTitle>
            </CardHeader>
            {!contentThemes.terms || contentThemes.terms.length === 0 ? (
              <p className="text-sm text-slate-400">Nothing in this window yet.</p>
            ) : (
              <div className="flex flex-wrap gap-2">
                {contentThemes.terms.map((t) => (
                  <span key={t.term} className="rounded-full bg-slate-800 px-3 py-1 text-sm" title={`${t.count}x`}>
                    {t.term}
                  </span>
                ))}
              </div>
            )}
          </Card>
        )}

        {narrative?.enabled && narrative.narrative && (
          <Card>
            <CardHeader>
              <CardTitle>Narrative summary (decrypted on view)</CardTitle>
            </CardHeader>
            <p className="text-sm">{narrative.narrative}</p>
          </Card>
        )}

        <Card>
          <CardHeader>
            <CardTitle>Memory health</CardTitle>
          </CardHeader>
          {health === null ? (
            <p className="text-sm text-slate-400">Loading…</p>
          ) : (
            <dl className="grid grid-cols-2 gap-y-1 text-sm">
              <dt className="text-slate-400">Episodes</dt>
              <dd>{health.episode_count}</dd>
              <dt className="text-slate-400">Summaries</dt>
              <dd>{health.summary_count}</dd>
              <dt className="text-slate-400">Corrections applied</dt>
              <dd>{health.corrections_count}</dd>
              <dt className="text-slate-400">Last summary</dt>
              <dd>{health.last_summary_at ? new Date(health.last_summary_at).toLocaleDateString() : "—"}</dd>
            </dl>
          )}
        </Card>

        <Card>
          <CardHeader>
            <CardTitle>Forgotten but important</CardTitle>
          </CardHeader>
          {forgotten === null ? (
            <p className="text-sm text-slate-400">Loading…</p>
          ) : forgotten.episodes.length === 0 && forgotten.entities.length === 0 ? (
            <p className="text-sm text-slate-400">Nothing flagged important has gone stale.</p>
          ) : (
            <ul className="space-y-1 text-sm">
              {forgotten.episodes.map((e) => (
                <li key={e.id} className="flex justify-between">
                  <span>High-importance conversation</span>
                  <span className="text-slate-400">{new Date(e.ts).toLocaleDateString()}</span>
                </li>
              ))}
              {forgotten.entities.map((e) => (
                <li key={e.id} className="flex justify-between">
                  <span>
                    {e.name} <span className="text-slate-500">({e.kind})</span>
                  </span>
                  <span className="text-slate-400">not touched since {e.last_updated}</span>
                </li>
              ))}
            </ul>
          )}
        </Card>

        <Card>
          <CardHeader>
            <CardTitle>Retrieval governance</CardTitle>
          </CardHeader>
          {governance === null ? (
            <p className="text-sm text-slate-400">Loading…</p>
          ) : (
            <p className="text-sm">
              Keyword search is currently <strong>{governance.tier}</strong> for this scope
              {governance.tier !== "full" && (
                <> ({governance.total_corpus_size} items, narrow at {governance.narrow_threshold}, disable at {governance.disable_threshold})</>
              )}
              .
            </p>
          )}
        </Card>

        <Card>
          <CardHeader>
            <CardTitle>Entity relationships</CardTitle>
          </CardHeader>
          {relationships === null ? (
            <p className="text-sm text-slate-400">Loading…</p>
          ) : relationships.length === 0 ? (
            <p className="text-sm text-slate-400">None yet.</p>
          ) : (
            <ul className="space-y-1 text-sm">
              {relationships.map((r, i) => (
                <li key={i}>
                  <span className="text-slate-300">{r.subject_id}</span>{" "}
                  <span className="text-slate-500">{r.predicate}</span>{" "}
                  <span className="text-slate-300">{r.object_id}</span>
                </li>
              ))}
            </ul>
          )}
        </Card>

        <Card>
          <CardHeader>
            <CardTitle>Security posture</CardTitle>
          </CardHeader>
          {security === null ? (
            <p className="text-sm text-slate-400">Loading…</p>
          ) : (
            <div className="space-y-2 text-sm">
              {security.key_rotation && (
                <p>
                  Key rotation: v{security.key_rotation.from_version} → v{security.key_rotation.to_version} (
                  {security.key_rotation.status})
                </p>
              )}
              {security.recent_events.length === 0 ? (
                <p className="text-slate-400">No export, import, correction, or key-rotation events.</p>
              ) : (
                <ul className="space-y-1">
                  {security.recent_events.map((e, i) => (
                    <li key={i} className="flex justify-between">
                      <span>{e.event_type}</span>
                      <span className="text-slate-400">{new Date(e.ts).toLocaleString()}</span>
                    </li>
                  ))}
                </ul>
              )}
            </div>
          )}
        </Card>
      </div>
    </div>
  );
}
