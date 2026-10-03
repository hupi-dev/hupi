import { HTMLAttributes } from "react";

// Same card shape the marketing site's own proof-point cards use
// (site/src/components/ProofPoints.astro) — navy surface over the ink
// page background, a hover glow instead of a static border, since this
// card is interactive dashboard content, not a static marketing tile.
export function Card({ className = "", ...props }: HTMLAttributes<HTMLDivElement>) {
  return (
    <div
      className={`rounded-xl border border-white/10 bg-navy-900/60 p-5 transition-colors hover:border-ember-500/30 ${className}`}
      {...props}
    />
  );
}

export function CardHeader({ className = "", ...props }: HTMLAttributes<HTMLDivElement>) {
  return <div className={`mb-3 flex flex-wrap items-center justify-between gap-2 ${className}`} {...props} />;
}

export function CardTitle({ className = "", ...props }: HTMLAttributes<HTMLHeadingElement>) {
  return <h2 className={`text-base font-semibold text-fog-100 ${className}`} {...props} />;
}

// Icon-in-accent-tinted-box wrapper, same pattern as the site's
// Pillars.astro (`flex h-11 w-11 items-center justify-center rounded-lg
// border border-ember-500/30 bg-ember-500/10 text-ember-500`), sized down
// slightly for a dashboard card header rather than a full pillar block.
export function CardIcon({ className = "", ...props }: HTMLAttributes<HTMLDivElement>) {
  return (
    <div
      className={`flex h-9 w-9 shrink-0 items-center justify-center rounded-lg border border-ember-500/30 bg-ember-500/10 text-ember-500 ${className}`}
      {...props}
    />
  );
}
