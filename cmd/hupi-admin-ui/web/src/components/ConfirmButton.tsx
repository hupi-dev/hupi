import { useState } from "react";
import { Button } from "./Button";

// A small inline "Are you sure?" confirm, used in place of a bare browser
// confirm() for destructive actions (revoke key / revoke operator) — kept
// visually consistent with the rest of the app instead of a native dialog.
export function ConfirmButton({
  label,
  confirmLabel = "Are you sure?",
  onConfirm,
  variant = "danger",
}: {
  label: string;
  confirmLabel?: string;
  onConfirm: () => void | Promise<void>;
  variant?: "danger" | "secondary";
}) {
  const [confirming, setConfirming] = useState(false);
  const [busy, setBusy] = useState(false);

  if (!confirming) {
    return (
      <Button variant={variant} className="px-2 py-1 text-xs" onClick={() => setConfirming(true)}>
        {label}
      </Button>
    );
  }

  return (
    <div className="flex items-center gap-2 rounded-md border border-slate-700 bg-slate-900 px-2 py-1">
      <span className="text-xs text-slate-300">{confirmLabel}</span>
      <Button
        variant={variant}
        className="px-2 py-0.5 text-xs"
        disabled={busy}
        onClick={async () => {
          setBusy(true);
          try {
            await onConfirm();
            setConfirming(false);
          } finally {
            setBusy(false);
          }
        }}
      >
        Yes
      </Button>
      <Button
        variant="ghost"
        className="px-2 py-0.5 text-xs"
        disabled={busy}
        onClick={() => setConfirming(false)}
      >
        Cancel
      </Button>
    </div>
  );
}
