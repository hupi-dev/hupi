# HUPI marketing site

A static, single-page marketing/landing site for HUPI, built with
[Astro](https://astro.build) + [Tailwind CSS](https://tailwindcss.com), with a
single React island (`src/islands/FlowDiagram.jsx`) for the "how it works"
diagram. Everything else ships as plain static HTML/CSS.

This directory is a fully independent npm project — it does not affect, and
is not affected by, the rest of the HUPI Go codebase.

## Run it locally

```bash
npm install
npm run dev
```

Opens a dev server (default `http://localhost:4321`) with hot reload.

## Build

```bash
npm run build
```

Outputs a fully static site to `dist/`. Check it with:

```bash
npm run preview
```

## Deploying

The output in `dist/` is plain static files — no server or serverless
function required. Deploy it with any of:

- **Vercel**: connect this repo (or `site/` as the project root) and set the
  build command to `npm run build` with output directory `dist`; Vercel
  auto-detects Astro.
- **Netlify**: same idea — either connect the repo and point the base
  directory at `site/` with build command `npm run build` and publish
  directory `dist`, or just drag the built `dist/` folder into Netlify's
  manual deploy UI for a quick one-off.
- **Docker** (self-hosted, e.g. alongside the rest of HUPI's deployment):

  ```bash
  docker build -t hupi-site .
  docker run --rm -p 8080:80 hupi-site
  ```

  A multi-stage build (`node:20-alpine` to build, `nginx:alpine` to serve
  the static output) — see `Dockerfile` in this directory. This is an
  independent, optional deployment path; it doesn't replace or require
  Vercel/Netlify, and isn't wired into the main HUPI `deploy/k8s/` or Helm
  chart (this site has no dependency on Postgres or anything else HUPI
  itself needs — it's a static page).

## Placeholders to replace before this goes live

Centralized in `src/lib/links.js`:

- `GITHUB_URL` — currently `https://github.com/REPLACE_ME/hupi`. Every
  other placeholder below is derived from or paired with this one.
- `DOCS_URL` — currently `${GITHUB_URL}/tree/main/docs` (no standalone docs
  site exists yet, so this points into the repo itself; update once one
  does).
- `CONTACT_EMAIL` — currently `hello@REPLACE_ME.example`.
- `LICENSE_LABEL` — currently `see repository` (linking to `GITHUB_URL`);
  no `LICENSE` file existed at the repository root at the time this site
  was built, so no specific license is asserted. Once one exists, change
  this to the real license name (e.g. `MIT`). Shown in the footer
  (`src/components/FinalCta.astro`).

`astro.config.mjs`'s `site` field (`https://example.com/REPLACE_ME`) should
also be updated to the real production URL once known — it's only used for
generating absolute URLs (e.g. in a future sitemap/RSS), and isn't required
for the build to succeed.
