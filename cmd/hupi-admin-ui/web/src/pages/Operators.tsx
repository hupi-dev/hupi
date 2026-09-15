import { FormEvent, useState } from "react";
import { Plus } from "lucide-react";
import { api, Operator } from "../lib/api";
import { useAsync } from "../lib/useAsync";
import { formatDate } from "../lib/format";
import { Card, CardHeader, CardTitle } from "../components/Card";
import { Table } from "../components/Table";
import { Badge } from "../components/Badge";
import { Button } from "../components/Button";
import { Modal } from "../components/Modal";
import { ConfirmButton } from "../components/ConfirmButton";
import { AsyncBoundary } from "../components/AsyncBoundary";
import { Field, inputClass } from "../components/Form";
import { useToast } from "../components/Toast";

export function Operators() {
  const { notify } = useToast();
  const operators = useAsync(() => api.listOperators(), []);
  const [showCreate, setShowCreate] = useState(false);
  const [newToken, setNewToken] = useState<string | null>(null);

  async function revoke(name: string) {
    try {
      await api.revokeOperator(name);
      notify("success", `Operator "${name}" revoked.`);
      operators.reload();
    } catch (err) {
      notify("error", err instanceof Error ? err.message : "Failed to revoke operator.");
    }
  }

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <h1 className="text-2xl font-semibold text-slate-100">Operators</h1>
        <Button onClick={() => setShowCreate(true)}>
          <Plus size={16} /> New operator
        </Button>
      </div>

      <Card>
        <CardHeader>
          <CardTitle>Admin UI credentials ({operators.data?.length ?? 0})</CardTitle>
        </CardHeader>
        <AsyncBoundary loading={operators.loading} error={operators.error}>
          <Table<Operator>
            columns={[
              { header: "Name", render: (o) => o.Name },
              { header: "Created", render: (o) => formatDate(o.CreatedAt) },
              {
                header: "Status",
                render: (o) =>
                  o.RevokedAt ? (
                    <Badge tone="red">Revoked {formatDate(o.RevokedAt)}</Badge>
                  ) : (
                    <Badge tone="green">Active</Badge>
                  ),
              },
              {
                header: "",
                render: (o) => !o.RevokedAt && <ConfirmButton label="Revoke" onConfirm={() => revoke(o.Name)} />,
              },
            ]}
            rows={operators.data ?? []}
            keyFn={(o) => o.Name}
            emptyMessage="No operators provisioned yet."
          />
        </AsyncBoundary>
      </Card>

      <CreateOperatorModal
        open={showCreate}
        onClose={() => setShowCreate(false)}
        onCreated={(token) => {
          setNewToken(token);
          operators.reload();
        }}
      />

      <Modal open={newToken !== null} onClose={() => setNewToken(null)} title="New operator token">
        <p className="mb-3 text-sm text-amber-400">Copy this token now — it cannot be shown again.</p>
        <code className="block break-all rounded-md border border-slate-700 bg-slate-950 p-3 text-sm text-slate-100">
          {newToken}
        </code>
        <div className="mt-4 flex justify-end">
          <Button onClick={() => setNewToken(null)}>Done</Button>
        </div>
      </Modal>
    </div>
  );
}

function CreateOperatorModal({
  open,
  onClose,
  onCreated,
}: {
  open: boolean;
  onClose: () => void;
  onCreated: (token: string) => void;
}) {
  const [name, setName] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function submit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      const { raw_token } = await api.createOperator(name.trim());
      onCreated(raw_token);
      setName("");
      onClose();
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to create operator.");
    } finally {
      setBusy(false);
    }
  }

  return (
    <Modal open={open} onClose={onClose} title="New operator">
      <form onSubmit={submit} className="space-y-3">
        <Field label="Name">
          <input className={inputClass} value={name} onChange={(e) => setName(e.target.value)} required placeholder="alice" />
        </Field>
        {error && <p className="text-sm text-red-400">{error}</p>}
        <div className="flex justify-end gap-2 pt-2">
          <Button type="button" variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button type="submit" disabled={busy || !name.trim()}>
            {busy ? "Creating…" : "Create"}
          </Button>
        </div>
      </form>
    </Modal>
  );
}
