# hupi-admin-ui web frontend

React + TypeScript + Vite + Tailwind single-page app for `cmd/hupi-admin-ui`.
It talks to the JSON API documented in [../../../docs/ADMIN_UI.md](../../../docs/ADMIN_UI.md)
under `/api/*`. See that doc for the auth model (HTTP Basic, operator
name + raw token), the CSRF mitigation, and the full route list.

## Local development

```bash
npm install
npm run dev
```

Vite's dev server proxies any request to `/api` through to a real running
`hupi-admin-ui` instance, so you can iterate on the frontend with hot
reload without rebuilding the Go binary each time. The proxy target
defaults to `http://127.0.0.1:8788` (the Go server's default listen
address); override it with:

```bash
HUPI_ADMIN_UI_DEV_PROXY_TARGET=http://127.0.0.1:9000 npm run dev
```

You need a real `hupi-admin-ui` process running somewhere (with at least
one operator provisioned via `hupi-admin create-operator`) for the app to
have anything to talk to — this only proxies API calls, it doesn't stub
them.

## Building

```bash
npm run build
```

Type-checks with `tsc -b` then builds with Vite, producing static assets
in `dist/`. `cmd/hupi-admin-ui/assets.go` embeds that directory into the
Go binary via `//go:embed web/dist` at Go compile time — so this npm
build has to run *before* `go build ./cmd/hupi-admin-ui` for the binary
to contain the real app.

`install.sh` (when `ADMIN_UI=1`) and the repo-root `Dockerfile` both run
this automatically. If you're building `hupi-admin-ui` by hand, run the
npm build first:

```bash
cd cmd/hupi-admin-ui/web && npm ci && npm run build
cd ../../.. && go build -o bin/hupi-admin-ui ./cmd/hupi-admin-ui
```

### Why `dist/` has a placeholder committed

`go:embed` needs the target directory to exist at compile time, but
`dist/` is npm build output, not source. A minimal placeholder
`dist/index.html` is committed (see `.gitignore` in this directory — it
ignores everything under `dist/` except that one file) so a plain
`go build ./cmd/hupi-admin-ui` on a completely fresh clone still succeeds
(serving that placeholder page instead of the real app) instead of
failing with a `go:embed` error. Running `npm run build` overwrites it
with the real app; that overwritten output isn't meant to be committed
back — only the placeholder is source-controlled.

## Testing

```bash
npm test
```

Runs Vitest + React Testing Library. Coverage here is intentionally light
(login flow, one CRUD screen) — the project's actual correctness bar for
this frontend is manual verification against a real `hupi-admin-ui`
instance, not an exhaustive automated suite. See ADMIN_UI.md for how
`cmd/hupi-admin-ui`'s own Go test suite covers the API this calls.

## Project structure

```
src/
  main.tsx          entry point, mounts <App/> under a BrowserRouter
  App.tsx           route table
  index.css         Tailwind entry point
  lib/
    api.ts          typed fetch client for /api/* (mirrors handlers.go's response shapes exactly)
    auth.tsx         AuthProvider/useAuth/RequireAuth — sessionStorage-backed Basic Auth credential
    session.ts       sessionStorage read/write for the stored credential
    useAsync.ts       shared fetch-on-mount loading/error/reload hook
    format.ts         date formatting helper
    auditEvents.ts    hardcoded list of internal/audit's Event* constants
  components/         hand-rolled UI primitives (Button, Card, Table, Badge, Modal, Toast, ConfirmButton, AsyncBoundary, Form, Layout)
  pages/              one file per route (Login, Dashboard, UserDetail, TeamDetail, Operators, Audit)
```

No component-kit dependency (no MUI/Chakra/shadcn/etc) — the primitives
in `components/` are small and hand-styled with Tailwind, reused across
every screen.
