# Contributing to HUPI

Thanks for looking at this. A few things worth knowing before you dive in.

## Scope: what belongs in this repo

This repo is Tier 1/2 — the single-user gateway, retrieval, consolidation,
encryption, and admin tooling. It's MIT-licensed and free forever, and
contributions here are genuinely welcome.

**Tier 3** (team/shared-workspace support: real end-user/team
authentication, the `/v1/team/...` routes, team CLI subcommands) lives in
a separate, privately-held, commercially-licensed repo (`hupi-t3`), not
this one — see [ARCHITECTURE.md § Licensing and the open-core
split](ARCHITECTURE.md) for why and how the two connect. If you're
looking to add or fix something team/workspace-related, it almost
certainly belongs there, not here — open an issue first if you're not
sure which side of the split something falls on.

## Setting up a dev environment

Follow [docs/INSTALL.md](docs/INSTALL.md)'s manual setup (Postgres +
pgvector, the numbered `schema/0*.sql` migrations, a `providers.yaml`).
`./install.sh` automates all of that if you'd rather not do it by hand.

## Running the test suites

**Go** (from the repo root):
```bash
go build ./...
go vet ./...
go test ./...
```
Most packages have unit tests that run with no setup. A handful are
integration tests against a real Postgres, skipped automatically unless
`HUPI_TEST_DATABASE_URL` is set:
```bash
export HUPI_TEST_DATABASE_URL='postgres://hupi_app:yourpassword@localhost:5432/hupi?sslmode=disable'
```
Use the `hupi_app` role, not a superuser/table-owner connection — Postgres
row-level security (`internal/store`'s RLS tests) is bypassed by
superusers and owners, so testing against one means those tests pass
without actually exercising RLS at all.

**VS Code extension** (from `vscode-extension/`):
```bash
npm ci
npm run typecheck
npm run lint
npm test
npm run build
```

**Marketing site** (from `site/`):
```bash
npm ci
npm run build
```

`.github/workflows/ci.yml` runs all three on every push and PR — if it's
green there, it's using the same commands above, not something different.

## Code style

- Go: matches the surrounding file — `gofmt` is enforced implicitly by
  convention, not a separate lint step. No comments explaining *what*
  code does (names should already say that); comments exist for *why*
  something non-obvious is the way it is.
- TypeScript (`vscode-extension/`): `npm run lint` (ESLint) and
  `npm run typecheck` (`tsc --noEmit`) both have to pass.
- [docs/CODE_GUIDE.md](docs/CODE_GUIDE.md) is a structural map of the
  actual code (what lives where, what calls what) — read it before
  making a non-trivial change, since it'll usually save you from
  rediscovering something already documented there.

## Commit messages / PRs

No strict format enforced, but look at `git log` for the actual house
style: a concise summary line (optionally `package:` or `area:` prefixed
when it helps), explaining *why* a change was made when that's not
obvious from the diff alone, not just restating *what* changed.

Keep PRs focused — one logical change per PR is easier to review than a
bundle of unrelated ones, even if they're all small.

## Reporting a bug vs. a security vulnerability

Regular bugs: open a GitHub Issue (a template will walk you through what's
useful to include). A security vulnerability: see
[SECURITY.md](SECURITY.md).
