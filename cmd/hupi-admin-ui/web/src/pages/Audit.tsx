import { FormEvent, useState } from "react";
import { Search } from "lucide-react";
import { api, AuditEntry, AuditQueryParams } from "../lib/api";
import { useAsync } from "../lib/useAsync";
import { formatDate } from "../lib/format";
import { AUDIT_EVENT_TYPES } from "../lib/auditEvents";
import { Card } from "../components/Card";
import { Table } from "../components/Table";
import { Button } from "../components/Button";
import { AsyncBoundary } from "../components/AsyncBoundary";
import { Field, inputClass, Select } from "../components/Form";

interface FilterForm {
  scopeKind: string;
  scopeOwner: string;
  actor: string;
  eventType: string;
  since: string; // datetime-local value, local time
  until: string;
  limit: string;
}

const emptyFilters: FilterForm = {
  scopeKind: "",
  scopeOwner: "",
  actor: "",
  eventType: "",
  since: "",
  until: "",
  limit: "100",
};

function toRFC3339(datetimeLocal: string): string | undefined {
  if (!datetimeLocal) return undefined;
  const d = new Date(datetimeLocal);
  if (Number.isNaN(d.getTime())) return undefined;
  return d.toISOString();
}

function toQuery(f: FilterForm): AuditQueryParams {
  return {
    scope_kind: f.scopeKind || undefined,
    scope_owner: f.scopeOwner.trim() || undefined,
    actor: f.actor.trim() || undefined,
    event_type: f.eventType || undefined,
    since: toRFC3339(f.since),
    until: toRFC3339(f.until),
    limit: f.limit ? Number(f.limit) : undefined,
  };
}

function DetailCell({ text }: { text: string }) {
  const [expanded, setExpanded] = useState(false);
  if (!text) return <span className="text-slate-600">—</span>;
  const isLong = text.length > 90;
  return (
    <div className="max-w-sm">
      <code className={`block whitespace-pre-wrap break-all text-xs text-slate-300 ${!expanded && isLong ? "line-clamp-2" : ""}`}>
        {text}
      </code>
      {isLong && (
        <button type="button" className="mt-1 text-xs text-accent hover:underline" onClick={() => setExpanded((e) => !e)}>
          {expanded ? "Show less" : "Show more"}
        </button>
      )}
    </div>
  );
}

export function Audit() {
  const [form, setForm] = useState<FilterForm>(emptyFilters);
  const [applied, setApplied] = useState<FilterForm>(emptyFilters);

  const entries = useAsync(() => api.queryAudit(toQuery(applied)), [applied]);

  function submit(e: FormEvent) {
    e.preventDefault();
    setApplied(form);
  }

  function reset() {
    setForm(emptyFilters);
    setApplied(emptyFilters);
  }

  return (
    <div className="space-y-6">
      <h1 className="text-2xl font-semibold text-slate-100">Audit Log</h1>

      <Card>
        <form onSubmit={submit} className="grid grid-cols-2 gap-3 md:grid-cols-4">
          <Field label="Scope kind">
            <Select value={form.scopeKind} onChange={(e) => setForm({ ...form, scopeKind: e.target.value })}>
              <option value="">Any</option>
              <option value="private">private</option>
              <option value="shared">shared</option>
            </Select>
          </Field>
          <Field label="Scope owner">
            <input
              className={inputClass}
              value={form.scopeOwner}
              onChange={(e) => setForm({ ...form, scopeOwner: e.target.value })}
              placeholder="user:bob"
            />
          </Field>
          <Field label="Actor">
            <input
              className={inputClass}
              value={form.actor}
              onChange={(e) => setForm({ ...form, actor: e.target.value })}
              placeholder="alice"
            />
          </Field>
          <Field label="Event type">
            <Select value={form.eventType} onChange={(e) => setForm({ ...form, eventType: e.target.value })}>
              <option value="">Any</option>
              {AUDIT_EVENT_TYPES.map((t) => (
                <option key={t} value={t}>
                  {t}
                </option>
              ))}
            </Select>
          </Field>
          <Field label="Since">
            <input
              type="datetime-local"
              className={inputClass}
              value={form.since}
              onChange={(e) => setForm({ ...form, since: e.target.value })}
            />
          </Field>
          <Field label="Until">
            <input
              type="datetime-local"
              className={inputClass}
              value={form.until}
              onChange={(e) => setForm({ ...form, until: e.target.value })}
            />
          </Field>
          <Field label="Limit">
            <input
              type="number"
              min={1}
              max={500}
              className={inputClass}
              value={form.limit}
              onChange={(e) => setForm({ ...form, limit: e.target.value })}
            />
          </Field>
          <div className="flex items-end gap-2">
            <Button type="submit">
              <Search size={16} /> Query
            </Button>
            <Button type="button" variant="ghost" onClick={reset}>
              Reset
            </Button>
          </div>
        </form>
      </Card>

      <Card>
        <AsyncBoundary loading={entries.loading} error={entries.error}>
          <Table<AuditEntry>
            columns={[
              { header: "Timestamp", render: (e) => formatDate(e.TS), className: "whitespace-nowrap" },
              { header: "Event type", render: (e) => <code className="text-xs">{e.EventType}</code> },
              { header: "Actor", render: (e) => e.Actor },
              {
                header: "Workspace scope",
                render: (e) => (
                  <span className="text-xs text-slate-400">
                    {e.WorkspaceScopeKind}:{e.WorkspaceScopeOwner}
                  </span>
                ),
              },
              { header: "Detail", render: (e) => <DetailCell text={e.Detail} /> },
            ]}
            rows={entries.data ?? []}
            keyFn={(e, i) => `${e.TS}-${e.Actor}-${e.EventType}-${i}`}
            emptyMessage="No matching audit events."
          />
        </AsyncBoundary>
      </Card>
    </div>
  );
}
