import { FormEvent, useState } from "react";
import { Link, useParams } from "react-router-dom";
import { ArrowLeft, UserPlus } from "lucide-react";
import { api, Membership } from "../lib/api";
import { useAsync } from "../lib/useAsync";
import { formatDate } from "../lib/format";
import { Card, CardHeader, CardTitle } from "../components/Card";
import { Table } from "../components/Table";
import { Button } from "../components/Button";
import { AsyncBoundary } from "../components/AsyncBoundary";
import { Field, inputClass } from "../components/Form";
import { useToast } from "../components/Toast";

export function TeamDetail() {
  const { id = "" } = useParams();
  const { notify } = useToast();
  const bundle = useAsync(() => api.getTeam(id), [id]);

  const [userId, setUserId] = useState("");
  const [role, setRole] = useState("");
  const [busy, setBusy] = useState(false);

  async function addMember(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    try {
      await api.addMember(id, userId.trim(), role.trim());
      notify("success", `Added ${userId.trim()} to the team.`);
      setUserId("");
      setRole("");
      bundle.reload();
    } catch (err) {
      notify("error", err instanceof Error ? err.message : "Failed to add member.");
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="space-y-6">
      <Link to="/" className="inline-flex items-center gap-1 text-sm text-slate-400 hover:text-slate-200">
        <ArrowLeft size={14} /> Back to dashboard
      </Link>

      <AsyncBoundary loading={bundle.loading} error={bundle.error}>
        {bundle.data && (
          <>
            <Card>
              <CardHeader>
                <CardTitle>{bundle.data.team.Name || bundle.data.team.ID}</CardTitle>
              </CardHeader>
              <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1 text-sm">
                <dt className="text-slate-500">ID</dt>
                <dd>
                  <code className="text-xs text-slate-400">{bundle.data.team.ID}</code>
                </dd>
                <dt className="text-slate-500">Created</dt>
                <dd>{formatDate(bundle.data.team.CreatedAt)}</dd>
              </dl>
            </Card>

            <Card>
              <CardHeader>
                <CardTitle>Members</CardTitle>
              </CardHeader>
              <Table<Membership>
                columns={[
                  {
                    header: "User",
                    render: (m) => (
                      <Link to={`/users/${encodeURIComponent(m.UserID)}`} className="text-accent hover:underline">
                        {m.UserID}
                      </Link>
                    ),
                  },
                  { header: "Role", render: (m) => m.Role || <span className="text-slate-600">—</span> },
                ]}
                rows={bundle.data.members}
                keyFn={(m) => m.UserID}
                emptyMessage="No members yet."
              />

              <form onSubmit={addMember} className="mt-4 flex flex-wrap items-end gap-3 border-t border-slate-800 pt-4">
                <Field label="User ID" className="w-48">
                  <input className={inputClass} value={userId} onChange={(e) => setUserId(e.target.value)} required placeholder="user:bob" />
                </Field>
                <Field label="Role" className="w-40">
                  <input className={inputClass} value={role} onChange={(e) => setRole(e.target.value)} placeholder="member" />
                </Field>
                <Button type="submit" disabled={busy || !userId.trim()}>
                  <UserPlus size={16} /> {busy ? "Adding…" : "Add member"}
                </Button>
              </form>
            </Card>
          </>
        )}
      </AsyncBoundary>
    </div>
  );
}
