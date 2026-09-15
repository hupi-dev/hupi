// Central place for placeholder values that a human must replace before
// this site goes live. Nothing else in the codebase should hardcode these.

export const GITHUB_URL = 'https://github.com/samuel-sujith/hupi';

// TODO(replace-before-launch): point at a real docs site/route once one
// exists. Deliberately not a same-origin "/docs" path — this site has no
// such route, and a dead same-origin link 404s immediately with no way to
// discover the mistake from the URL alone. Routing through GITHUB_URL
// instead means the link is broken in exactly the same fixable way
// GITHUB_URL itself already is: replace GITHUB_URL and this stays correct.
export const DOCS_URL = `${GITHUB_URL}/tree/main/docs`;

// TODO(replace-before-launch): real contact address.
export const CONTACT_EMAIL = 'hello@REPLACE_ME.example';

// No LICENSE file was found at the repository root at the time this site
// was built, so no specific license is asserted here — that text renders
// on the live page, and showing visitors an internal "unconfirmed" note
// would be its own bug. Once a real LICENSE file exists, replace this with
// the actual license name (e.g. 'MIT').
export const LICENSE_LABEL = 'see repository';
