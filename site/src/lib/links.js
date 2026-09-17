// Central place for placeholder values that a human must replace before
// this site goes live. Nothing else in the codebase should hardcode these.

export const GITHUB_URL = 'https://github.com/samuel-sujith/hupi';

// Tier 3 (team/shared-workspace support) lives in a separate,
// commercially-licensed repo, not this one — see ARCHITECTURE.md §
// "Licensing and the open-core split" for why and how the two connect.
export const TIER3_REPO_URL = 'https://github.com/samuel-sujith/hupi-t3';

// The site's own /docs page (src/pages/docs.astro) is the primary "Docs"
// destination in nav/footer/hero now — this is only a deep link into the
// docs/ directory listing on GitHub, used from within /docs, /faq, and
// /compare for "the full reference" links.
export const DOCS_URL = `${GITHUB_URL}/tree/main/docs`;

// GitHub's file-view URL for one specific docs/*.md file — "tree" (used
// by DOCS_URL) is for directory listings, "blob" is for an individual
// file; using tree for a file works via GitHub's own redirect but isn't
// the URL GitHub actually generates, so this builds the correct one
// directly rather than relying on that redirect for permanent site copy.
export const docFileURL = (name) => `${GITHUB_URL}/blob/main/docs/${name}`;

export const CONTACT_EMAIL = 'work@hupi.dev';

export const VSCODE_MARKETPLACE_URL =
  'https://marketplace.visualstudio.com/items?itemName=hupi.hupi-vscode';

export const LICENSE_LABEL = 'MIT';
export const LICENSE_URL = `${GITHUB_URL}/blob/main/LICENSE`;
