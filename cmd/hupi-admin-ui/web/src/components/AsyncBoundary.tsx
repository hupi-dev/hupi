import { ReactNode } from "react";
import { AlertTriangle, Loader2 } from "lucide-react";

// Shared loading/error rendering so every screen gets a real state instead
// of silently rendering nothing (the old server-rendered templates had no
// such states at all).
export function AsyncBoundary({
  loading,
  error,
  children,
}: {
  loading: boolean;
  error: string | null;
  children: ReactNode;
}) {
  if (loading) {
    return (
      <div className="flex items-center justify-center gap-2 p-8 text-sm text-slate-500">
        <Loader2 size={16} className="animate-spin" /> Loading…
      </div>
    );
  }
  if (error) {
    return (
      <div className="flex items-start gap-2 rounded-md border border-red-900 bg-red-950/40 p-4 text-sm text-red-300">
        <AlertTriangle size={16} className="mt-0.5 shrink-0" />
        <span>Failed to load: {error}</span>
      </div>
    );
  }
  return <>{children}</>;
}
