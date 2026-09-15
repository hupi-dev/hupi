import { ReactNode } from "react";

type Tone = "green" | "red" | "slate" | "amber";

const tones: Record<Tone, string> = {
  green: "bg-emerald-500/15 text-emerald-400 ring-emerald-500/30",
  red: "bg-red-500/15 text-red-400 ring-red-500/30",
  slate: "bg-slate-500/15 text-slate-300 ring-slate-500/30",
  amber: "bg-amber-500/15 text-amber-400 ring-amber-500/30",
};

export function Badge({ tone = "slate", children }: { tone?: Tone; children: ReactNode }) {
  return (
    <span className={`inline-flex items-center rounded-full px-2 py-0.5 text-xs font-medium ring-1 ring-inset ${tones[tone]}`}>
      {children}
    </span>
  );
}
