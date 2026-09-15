# HUPI

HUPI is a **memory gateway**: a small, self-hosted service that sits between
your existing AI client and whichever LLM vendor you use. It speaks the
OpenAI Chat Completions API on both sides, so any existing client works
unmodified — point its `base_url` at HUPI instead of OpenAI/Anthropic/etc.
Underneath, every turn is enriched with relevant memory before being
forwarded to the real AI, and durably recorded afterward for future turns to
draw on — encrypted, multi-tenant, provider-independent, and auditable.

Three properties it's built around:

1. **Provider-independence** — switch from GPT to Claude to a locally hosted
   model without migrating or re-indexing your memory.
2. **Integrity over convenience** — every "memory" is independently
   fact-checked against its source before being trusted, rather than
   confidently made up.
3. **Auditability** — every retrieval decision is recorded and later
   inspectable: what exactly was remembered, and from where.

See [docs/BUSINESS_PROCESS.md](docs/BUSINESS_PROCESS.md) for the full
non-technical explanation, or [ARCHITECTURE.md](ARCHITECTURE.md) for the
design.

## Repository layout

```
cmd/                  Go binaries — the gateway, cron jobs, and CLIs
  hupi/                 the gateway server itself (long-lived, serves the HTTP API)
  hupi-admin-ui/         operator admin console — JSON API (Go) + React frontend
    web/                   the React admin console source (Vite + TypeScript + Tailwind)
  hupi-consolidate/      nightly consolidation + rollups (cron)
  hupi-selfcheck/        retrieval regression checks (cron)
  hupi-admin/            CLI: provision users/teams/API keys/operators
  hupi-audit/            CLI: query the audit log
  hupi-export/           CLI: write a portable, encrypted memory snapshot
  hupi-import/           CLI: load a snapshot back in
  hupi-rotate-key/       CLI: online, resumable per-scope key rotation
  hupi-trace/            CLI: inspect one episode's retrieval trace
  hupi-correct/          CLI: write a corrected, superseding summary
internal/             Go packages implementing the gateway, storage, crypto, etc.
schema/               numbered Postgres migrations, applied in order
deploy/k8s/           plain Kubernetes manifests
deploy/helm/hupi/     the same, as a Helm chart
site/                 marketing/landing website (Astro + Tailwind) — see site/README.md
docs/                 design docs, install guide, API reference, code guide
install.sh            interactive/scriptable bare-metal installer
Dockerfile            one image containing every Go binary above
```

**Marketing website code**: [site/](site/) — an independent Astro +
Tailwind static site, not part of the Go build. See
[site/README.md](site/README.md) for how to run, build, and deploy it.

**Admin UI code**: [cmd/hupi-admin-ui/](cmd/hupi-admin-ui/) — the Go JSON
API backend, with its React frontend in
[cmd/hupi-admin-ui/web/](cmd/hupi-admin-ui/web/). The frontend is built
separately (`npm run build`) and embedded into the Go binary via
`go:embed`; see [docs/ADMIN_UI.md](docs/ADMIN_UI.md) for the full picture.

## Getting started

```bash
./install.sh          # interactive: sets up Postgres, schema, keys, binaries
```

See [docs/INSTALL.md](docs/INSTALL.md) for the full manual walkthrough
(bare-metal) or containerized deployment (Docker/Kubernetes/Helm).

## Documentation

| Doc | Covers |
|---|---|
| [docs/BUSINESS_PROCESS.md](docs/BUSINESS_PROCESS.md) | What HUPI is and does, for a non-code audience |
| [ARCHITECTURE.md](ARCHITECTURE.md) | System design and the reasoning behind it |
| [docs/INSTALL.md](docs/INSTALL.md) | Bare-metal and containerized install |
| [docs/API_REFERENCE.md](docs/API_REFERENCE.md) | The gateway's HTTP routes, call chain by call chain |
| [docs/ADMIN_UI.md](docs/ADMIN_UI.md) | The operator admin console |
| [docs/CODE_GUIDE.md](docs/CODE_GUIDE.md) | Project layout and package dependency graph |
| [docs/MEMORY_FORMAT.md](docs/MEMORY_FORMAT.md) | The portable memory format (HPMF) |

## Status

This is a private, in-development project — see
[docs/GAP_CLOSURE_PLAN.md](docs/GAP_CLOSURE_PLAN.md) for what's been closed
recently and [docs/DESIGN_VS_BUILT.md](docs/DESIGN_VS_BUILT.md) for an
honest accounting of design vs. what's actually implemented. No `LICENSE`
file exists yet.
