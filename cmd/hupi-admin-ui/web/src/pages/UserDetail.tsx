import { FormEvent, useState } from "react";
import { Link, useParams } from "react-router-dom";
import { ArrowLeft, KeyRound } from "lucide-react";
import { api, APIKey, Team } from "../lib/api";
import { useAsync } from "../lib/useAsync";
import { formatDate } from "../lib/format";
import { Card, CardHeader, CardTitle } from "../components/Card";
import { Table } from "../components/Table";
import { Badge } from "../components/Badge";
import { Button } from "../components/Button";
import { Field, TextInput } from "../components/Form";
import { Modal } from "../components/Modal";
import { ConfirmButton } from "../components/ConfirmButton";
import { AsyncBoundary } from "../components/AsyncBoundary";
import { useToast } from "../components/Toast";

export function UserDetail() {
  const { id = "" } = useParams();
  const { notify } = useToast();
  const bundle = useAsync(() => api.getUser(id), [id]);
  const [newKey, setNewKey] = useState<string | null>(null);
  const [issuing, setIssuing] = useState(false);
  const [password, setPasswordInput] = useState("");
  const [settingPassword, setSettingPassword] = useState(false);

  async function submitPassword(e: FormEvent) {
    e.preventDefault();
    setSettingPassword(true);
    try {
      await api.setPassword(id, password);
      notify("success", "Dashboard password set.");
      setPasswordInput("");
    } catch (err) {
      notify("error", err instanceof Error ? err.message : "Failed to set password.");
    } finally {
      setSettingPassword(false);
    }
  }

  async function issueKey() {
    setIssuing(true);
    try {
      const { raw_key } = await api.issueKey(id);
      setNewKey(raw_key);
      bundle.reload();
    } catch (err) {
      notify("error", err instanceof Error ? err.message : "Failed to issue key.");
    } finally {
      setIssuing(false);
    }
  }

  async function revokeKey(keyHash: string) {
    try {
      await api.revokeKey(keyHash, id);
      notify("success", "Key revoked.");
      bundle.reload();
    } catch (err) {
      notify("error", err instanceof Error ? err.message : "Failed to revoke key.");
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
                <CardTitle>{bundle.data.user.ID}</CardTitle>
              </CardHeader>
              <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1 text-sm">
                <dt className="text-slate-500">Email</dt>
                <dd>{bundle.data.user.Email || "—"}</dd>
                <dt className="text-slate-500">Created</dt>
                <dd>{formatDate(bundle.data.user.CreatedAt)}</dd>
              </dl>
            </Card>

            <Card>
              <CardHeader>
                <CardTitle>Dashboard password</CardTitle>
              </CardHeader>
              <p className="mb-3 text-sm text-slate-400">
                Sets this user's password for hupi-dashboard's login screen (Tier 3 only) — no self-service reset,
                set it again here whenever it needs to change.
              </p>
              <form onSubmit={submitPassword} className="flex items-end gap-3">
                <Field label="New password" className="flex-1">
                  <TextInput
                    type="password"
                    value={password}
                    onChange={(e) => setPasswordInput(e.target.value)}
                    autoComplete="new-password"
                    required
                  />
                </Field>
                <Button type="submit" disabled={settingPassword || password === ""}>
                  {settingPassword ? "Setting…" : "Set password"}
                </Button>
              </form>
            </Card>

            <Card>
              <CardHeader>
                <CardTitle>Team memberships</CardTitle>
              </CardHeader>
              <Table<Team>
                columns={[
                  {
                    header: "Team",
                    render: (t) => (
                      <Link to={`/teams/${encodeURIComponent(t.ID)}`} className="text-accent hover:underline">
                        {t.Name || t.ID}
                      </Link>
                    ),
                  },
                  { header: "Created", render: (t) => formatDate(t.CreatedAt) },
                ]}
                rows={bundle.data.teams}
                keyFn={(t) => t.ID}
                emptyMessage="Not a member of any team."
              />
            </Card>

            <Card>
              <CardHeader>
                <CardTitle>API keys</CardTitle>
                <Button onClick={issueKey} disabled={issuing}>
                  <KeyRound size={16} /> {issuing ? "Issuing…" : "Issue new key"}
                </Button>
              </CardHeader>
              <Table<APIKey>
                columns={[
                  { header: "Key hash", render: (k) => <code className="text-xs text-slate-400">{k.KeyHash}</code> },
                  { header: "Created", render: (k) => formatDate(k.CreatedAt) },
                  {
                    header: "Status",
                    render: (k) =>
                      k.RevokedAt ? (
                        <Badge tone="red">Revoked {formatDate(k.RevokedAt)}</Badge>
                      ) : (
                        <Badge tone="green">Active</Badge>
                      ),
                  },
                  {
                    header: "",
                    render: (k) =>
                      !k.RevokedAt && <ConfirmButton label="Revoke" onConfirm={() => revokeKey(k.KeyHash)} />,
                  },
                ]}
                rows={bundle.data.keys}
                keyFn={(k) => k.KeyHash}
                emptyMessage="No API keys issued."
              />
            </Card>
          </>
        )}
      </AsyncBoundary>

      <Modal open={newKey !== null} onClose={() => setNewKey(null)} title="New API key">
        <p className="mb-3 text-sm text-amber-400">Copy this key now — it cannot be shown again.</p>
        <code className="block break-all rounded-md border border-slate-700 bg-slate-950 p-3 text-sm text-slate-100">
          {newKey}
        </code>
        <div className="mt-4 flex justify-end">
          <Button onClick={() => setNewKey(null)}>Done</Button>
        </div>
      </Modal>
    </div>
  );
}
