import { useEffect, useState } from "react";
import {
  api,
  ConversationVolumePoint,
  EntityRelationship,
  KeywordSearchGovernance,
  MemoryHealth,
  SecurityPosture,
  ThemeWordCloudEntry,
  Whoami,
} from "../lib/api";
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
  const governance = useLoaded<KeywordSearchGovernance>(() => api.keywordSearchGovernance());
  const security = useLoaded<SecurityPosture>(() => api.securityPosture());

  return (
    <div className="mx-auto max-w-5xl space-y-4 p-6">
      <header className="flex items-center justify-between">
        <h1 className="text-xl font-semibold">hupi-dashboard</h1>
        <span className="text-sm text-slate-400">{whoami.scope_kind}:{whoami.scope_owner}</span>
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
