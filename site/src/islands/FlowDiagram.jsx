import { useEffect, useRef, useState } from 'react';

const STEPS = [
  {
    key: 'capture',
    label: 'Capture',
    detail:
      "Every conversation turn is durably recorded the moment the AI replies — encrypted, by default, forever, until you deliberately delete it.",
  },
  {
    key: 'consolidate',
    label: 'Consolidate',
    detail:
      'Overnight, a separate job turns that day’s raw conversations into a distilled, cited summary — not the live chat path.',
  },
  {
    key: 'ground',
    label: 'Ground',
    detail:
      "A second, independent AI call fact-checks every claim in that summary against the original conversation before it's trusted. Anything unverifiable is flagged and excluded.",
  },
  {
    key: 'retrieve',
    label: 'Retrieve',
    detail:
      'A later question is matched against searchable summaries and standing entities — not a scan of every raw conversation you’ve ever had.',
  },
  {
    key: 'inject',
    label: 'Inject',
    detail:
      'The relevant memory is handed to the AI before it answers your next message — the model doesn’t have to "remember" on its own.',
  },
];

const AUTO_ADVANCE_MS = 3800;

export default function FlowDiagram() {
  const [active, setActive] = useState(0);
  const [paused, setPaused] = useState(false);
  const timerRef = useRef(null);

  useEffect(() => {
    if (paused) return undefined;
    timerRef.current = window.setInterval(() => {
      setActive((prev) => (prev + 1) % STEPS.length);
    }, AUTO_ADVANCE_MS);
    return () => window.clearInterval(timerRef.current);
  }, [paused]);

  return (
    <div
      className="w-full"
      onMouseEnter={() => setPaused(true)}
      onMouseLeave={() => setPaused(false)}
      onFocus={() => setPaused(true)}
      onBlur={() => setPaused(false)}
    >
      <div className="relative flex flex-col gap-3 md:flex-row md:items-stretch md:gap-2">
        {STEPS.map((step, i) => {
          const isActive = i === active;
          return (
            <div key={step.key} className="flex flex-1 items-center md:flex-col">
              <button
                type="button"
                onClick={() => setActive(i)}
                aria-pressed={isActive}
                className={[
                  'group flex w-full items-center gap-3 rounded-lg border px-4 py-3 text-left transition-all duration-300 md:flex-col md:items-center md:text-center',
                  isActive
                    ? 'border-ember-500/60 bg-ember-500/10 shadow-[0_0_0_1px_rgba(255,138,76,0.25)]'
                    : 'border-white/10 bg-navy-900/60 hover:border-white/25',
                ].join(' ')}
              >
                <span
                  className={[
                    'font-mono-tight flex h-8 w-8 shrink-0 items-center justify-center rounded-full border text-sm font-semibold transition-colors',
                    isActive
                      ? 'border-ember-400 bg-ember-500 text-ink'
                      : 'border-white/20 text-fog-300 group-hover:text-fog-100',
                  ].join(' ')}
                >
                  {i + 1}
                </span>
                <span
                  className={[
                    'text-sm font-semibold transition-colors md:mt-1',
                    isActive ? 'text-fog-100' : 'text-fog-300 group-hover:text-fog-100',
                  ].join(' ')}
                >
                  {step.label}
                </span>
              </button>

              {i < STEPS.length - 1 && (
                <div
                  aria-hidden="true"
                  className="mx-1 hidden h-px flex-1 self-center bg-gradient-to-r from-white/15 to-white/5 md:block"
                />
              )}
            </div>
          );
        })}
      </div>

      <div
        className="mt-6 min-h-[5.5rem] rounded-xl border border-white/10 bg-navy-900/40 p-5 transition-colors"
        role="status"
        aria-live="polite"
      >
        <p className="font-mono-tight text-xs uppercase tracking-widest text-ember-500">
          {STEPS[active].label}
        </p>
        <p className="mt-2 text-sm leading-relaxed text-fog-300">{STEPS[active].detail}</p>
      </div>

      <div className="mt-4 flex justify-center gap-1.5 md:justify-start" aria-hidden="true">
        {STEPS.map((step, i) => (
          <span
            key={step.key}
            className={[
              'h-1 w-6 rounded-full transition-colors duration-300',
              i === active ? 'bg-ember-500' : 'bg-white/10',
            ].join(' ')}
          />
        ))}
      </div>
    </div>
  );
}
