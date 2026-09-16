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

  A multi-stage build (`node:22-alpine` to build — Astro requires Node
  >=22.12 — `nginx:alpine` to serve the static output) — see `Dockerfile`
  in this directory. This is an independent, optional deployment path; it
  doesn't replace or require Vercel/Netlify, and isn't wired into the main
  HUPI `deploy/k8s/` or Helm chart (this site has no dependency on
  Postgres or anything else HUPI itself needs — it's a static page).

## Placeholders

Centralized in `src/lib/links.js`. All resolved:

- `GITHUB_URL` — `https://github.com/samuel-sujith/hupi` (public).
- `DOCS_URL` — `${GITHUB_URL}/tree/main/docs` (no standalone docs site
  exists yet, so this points into the repo itself).
- `CONTACT_EMAIL` — `samuel.sujith@gmail.com`.
- `LICENSE_LABEL`/`LICENSE_URL` — MIT, linked from the footer
  (`src/components/FinalCta.astro`) to the repo's `LICENSE` file.

## Live deployment

Deployed at [hupi.dev](https://hupi.dev) (Vercel project `hupi/site`,
auto-deploys on every push to `main` via the GitHub integration — see the
root-level `vercel.json` for why that's needed in this monorepo).

`astro.config.mjs`'s `site` field is `https://hupi.dev` — every absolute
URL below is derived from it (via `Astro.site` in `Layout.astro`), so
this is the one place a future domain change needs to happen.

## SEO

- **`site: 'https://hupi.dev'`** (`astro.config.mjs`) is the canonical
  origin `@astrojs/sitemap` and `Layout.astro` build every absolute URL
  from — sitemap entries, `<link rel="canonical">`, and Open
  Graph/Twitter image URLs. Get this wrong (or leave it as a placeholder)
  and you're telling search engines to trust the wrong URL, which is
  worse than not telling them anything.
- **`@astrojs/sitemap`** generates `sitemap-index.xml`/`sitemap-0.xml` at
  build time — zero maintenance as pages are added later.
- **`public/robots.txt`** allows everything and points at the sitemap.
- **`public/og-image.png`** (1200×630, generated from
  `scripts/og-image-source.svg` via `sharp` — regenerate with
  `node -e "require('sharp')('scripts/og-image-source.svg').resize(1200,630).png().toFile('public/og-image.png')"`
  after editing the source SVG) is what Slack/Discord/Twitter/etc. render
  when this URL is shared, and matters for click-through from search
  results that show a preview too.
- **JSON-LD structured data** (`SoftwareApplication`, in `Layout.astro`)
  gives search engines an unambiguous, machine-readable description of
  what this is, separate from parsing prose.
- Every page needs `<title>`/`<meta name="description">` — currently just
  the one page, using `Layout.astro`'s defaults. If more pages are added,
  pass `title`/`description` props per page rather than reusing the
  landing page's.
