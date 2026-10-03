import { HTMLAttributes } from "react";

export function Card({ className = "", ...props }: HTMLAttributes<HTMLDivElement>) {
  return <div className={`rounded-lg border border-slate-800 bg-slate-900/60 p-4 ${className}`} {...props} />;
}

export function CardHeader({ className = "", ...props }: HTMLAttributes<HTMLDivElement>) {
  return <div className={`mb-3 flex flex-wrap items-center justify-between gap-2 ${className}`} {...props} />;
}

export function CardTitle({ className = "", ...props }: HTMLAttributes<HTMLHeadingElement>) {
  return <h2 className={`text-base font-semibold text-slate-100 ${className}`} {...props} />;
}
