import { defineConfig } from 'astro/config';
import react from '@astrojs/react';
import sitemap from '@astrojs/sitemap';
import tailwindcss from '@tailwindcss/vite';

// `site` is the canonical production origin — Astro uses it to generate
// sitemap.xml, and Layout.astro reads it (via Astro.site) for canonical
// URLs and absolute Open Graph/Twitter image URLs. Must be the real,
// final domain: a wrong value here is worse than none, since it gets
// baked into URLs search engines are told to trust.
export default defineConfig({
  site: 'https://hupi.dev',
  integrations: [react(), sitemap()],
  vite: {
    plugins: [tailwindcss()],
  },
});
