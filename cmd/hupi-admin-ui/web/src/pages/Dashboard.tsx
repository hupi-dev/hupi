import { FormEvent, ReactNode, useEffect, useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { Plus, Search } from "lucide-react";
import { api, Team, User } from "../lib/api";
import { useAsync } from "../lib/useAsync";
import { formatDate } from "../lib/format";
import { Card } from "../components/Card";
import { Button } from "../components/Button";
import { Table } from "../components/Table";
import { Modal } from "../components/Modal";
import { AsyncBoundary } from "../components/AsyncBoundary";
import { Field, inputClass } from "../components/Form";
import { useToast } from "../components/Toast";

type Tab = "users" | "teams";

function matches(query: string, ...fields: string[]): boolean {
  if (!query.trim()) return true;
  const q = query.toLowerCase();
  return fields.some((f) => f.toLowerCase().includes(q));
}

function TabButton({ active, onClick, children }: { active: boolean; onClick: () => void; children: ReactNode }) {
  return (
    <button
      onClick={onClick}
      className={`-mb-px border-b-2 px-3 py-2 text-sm font-medium transition-colors ${
        active ? "border-accent text-accent" : "border-transparent text-slate-400 hover:text-slate-200"
      }`}
    >
      {children}
    </button>
  );
}

export function Dashboard() {
  const [tab, setTab] = useState<Tab>("users");
  const [query, setQuery] = useState("");
  const [showCreateUser, setShowCreateUser] = useState(false);
  const [showCreateTeam, setShowCreateTeam] = useState(false);

  const users = useAsync(() => api.listUsers(), []);
  const teams = useAsync(() => api.listTeams(), []);

  const filteredUsers = useMemo(
    () => (users.data ?? []).filter((u) => matches(query, u.ID, u.Email)),
    [users.data, query],
  );
  const filteredTeams = useMemo(
    () => (teams.data ?? []).filter((t) => matches(query, t.ID, t.Name)),
    [teams.data, query],
  );

  return (
    <div className="space-y-6">
      <h1 className="text-2xl font-semibold text-slate-100">Dashboard</h1>

      <div className="flex gap-2 border-b border-slate-800">
        <TabButton active={tab === "users"} onClick={() => setTab("users")}>
          Users ({users.data?.length ?? 0})
        </TabButton>
        <TabButton active={tab === "teams"} onClick={() => setTab("teams")}>
          Teams ({teams.data?.length ?? 0})
        </TabButton>
      </div>

      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="relative w-full max-w-sm">
          <Search size={16} className="pointer-events-none absolute left-2.5 top-1/2 -translate-y-1/2 text-slate-500" />
          <input
            className={`${inputClass} pl-8`}
            placeholder={tab === "users" ? "Search users by id or email…" : "Search teams by id or name…"}
            value={query}
            onChange={(e) => setQuery(e.target.value)}
          />
        </div>
        <Button onClick={() => (tab === "users" ? setShowCreateUser(true) : setShowCreateTeam(true))}>
          <Plus size={16} /> {tab === "users" ? "New user" : "New team"}
        </Button>
      </div>

      {tab === "users" ? (
        <Card>
          <AsyncBoundary loading={users.loading} error={users.error}>
            <Table<User>
              columns={[
                {
                  header: "ID",
                  render: (u) => (
                    <Link to={`/users/${encodeURIComponent(u.ID)}`} className="text-accent hover:underline">
                      {u.ID}
                    </Link>
                  ),
                },
                { header: "Email", render: (u) => u.Email || <span className="text-slate-600">—</span> },
                { header: "Created", render: (u) => formatDate(u.CreatedAt) },
              ]}
              rows={filteredUsers}
              keyFn={(u) => u.ID}
              emptyMessage={users.data && users.data.length > 0 ? "No users match your search." : "No users yet."}
            />
          </AsyncBoundary>
        </Card>
      ) : (
        <Card>
          <AsyncBoundary loading={teams.loading} error={teams.error}>
            <Table<Team>
              columns={[
                {
                  header: "Name",
                  render: (t) => (
                    <Link to={`/teams/${encodeURIComponent(t.ID)}`} className="text-accent hover:underline">
                      {t.Name || t.ID}
                    </Link>
                  ),
                },
                { header: "ID", render: (t) => <code className="text-xs text-slate-400">{t.ID}</code> },
                { header: "Created", render: (t) => formatDate(t.CreatedAt) },
              ]}
              rows={filteredTeams}
              keyFn={(t) => t.ID}
              emptyMessage={teams.data && teams.data.length > 0 ? "No teams match your search." : "No teams yet."}
            />
          </AsyncBoundary>
        </Card>
      )}

      <CreateUserModal open={showCreateUser} onClose={() => setShowCreateUser(false)} onCreated={users.reload} />
      <CreateTeamModal open={showCreateTeam} onClose={() => setShowCreateTeam(false)} onCreated={teams.reload} />
    </div>
  );
}

function CreateUserModal({ open, onClose, onCreated }: { open: boolean; onClose: () => void; onCreated: () => void }) {
  const { notify } = useToast();
  const [id, setId] = useState("");
  const [email, setEmail] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (open) {
      setId("");
      setEmail("");
      setError(null);
    }
  }, [open]);

  async function submit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      await api.createUser(id.trim(), email.trim());
      notify("success", `User "${id.trim()}" created.`);
      onCreated();
      onClose();
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to create user.");
    } finally {
      setBusy(false);
    }
  }

  return (
    <Modal open={open} onClose={onClose} title="New user">
      <form onSubmit={submit} className="space-y-3">
        <Field label="User ID">
          <input className={inputClass} value={id} onChange={(e) => setId(e.target.value)} required placeholder="user:bob" />
        </Field>
        <Field label="Email (optional)">
          <input
            className={inputClass}
            value={email}
            onChange={(e) => setEmail(e.target.value)}
            placeholder="bob@example.com"
            type="email"
          />
        </Field>
        {error && <p className="text-sm text-red-400">{error}</p>}
        <div className="flex justify-end gap-2 pt-2">
          <Button type="button" variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button type="submit" disabled={busy || !id.trim()}>
            {busy ? "Creating…" : "Create"}
          </Button>
        </div>
      </form>
    </Modal>
  );
}

function CreateTeamModal({ open, onClose, onCreated }: { open: boolean; onClose: () => void; onCreated: () => void }) {
  const { notify } = useToast();
  const [id, setId] = useState("");
  const [name, setName] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (open) {
      setId("");
      setName("");
      setError(null);
    }
  }, [open]);

  async function submit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      await api.createTeam(id.trim(), name.trim());
      notify("success", `Team "${name.trim()}" created.`);
      onCreated();
      onClose();
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to create team.");
    } finally {
      setBusy(false);
    }
  }

  return (
    <Modal open={open} onClose={onClose} title="New team">
      <form onSubmit={submit} className="space-y-3">
        <Field label="Team ID">
          <input className={inputClass} value={id} onChange={(e) => setId(e.target.value)} required placeholder="team:eng" />
        </Field>
        <Field label="Name">
          <input className={inputClass} value={name} onChange={(e) => setName(e.target.value)} required placeholder="Engineering" />
        </Field>
        {error && <p className="text-sm text-red-400">{error}</p>}
        <div className="flex justify-end gap-2 pt-2">
          <Button type="button" variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button type="submit" disabled={busy || !id.trim() || !name.trim()}>
            {busy ? "Creating…" : "Create"}
          </Button>
        </div>
      </form>
    </Modal>
  );
}
