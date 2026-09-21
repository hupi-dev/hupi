// Google Ads conversion tracking for hupi.dev only — never loaded,
// referenced, or shipped in any HUPI product (the gateway, the VS Code
// extension, HUPI Code). See src/pages/privacy.astro for the disclosure
// this file is required to match.
//
// Everything here is a safe no-op until a human fills in the real
// values below — no tag loads, no cookie gets set, and no conversion
// fires with a blank CONVERSION_ID, so this can ship to production
// before Google Ads has actually issued one.
//
// Fill these in once created in Google Ads (Tools & Settings ->
// Conversions): CONVERSION_ID is the one shared "AW-XXXXXXXXX" tag ID
// for the whole account; each entry in CONVERSION_LABELS is the
// per-action label Google generates for that specific conversion goal.
export const CONVERSION_ID = 'AW-18459468379';
export const CONVERSION_LABELS = {
	leadForm: '',
	githubClick: '',
	marketplaceClick: '',
	contactClick: '',
	docsEngagement: '',
};

const CONSENT_KEY = 'hupi-ads-consent';

export function getConsent() {
	try {
		return localStorage.getItem(CONSENT_KEY);
	} catch {
		return null;
	}
}

export function setConsent(value) {
	try {
		localStorage.setItem(CONSENT_KEY, value);
	} catch {
		// Private browsing / storage disabled — consent just won't persist
		// across reloads, which defaults safely back to "ask again."
	}
}

let gtagLoaded = false;

// Loads the Google tag exactly once, only after explicit consent, and
// only if a real conversion ID has been configured above.
export function loadGoogleAds() {
	if (gtagLoaded || !CONVERSION_ID) return;
	gtagLoaded = true;

	window.dataLayer = window.dataLayer || [];
	window.gtag = function gtag() {
		window.dataLayer.push(arguments);
	};
	window.gtag('js', new Date());
	window.gtag('config', CONVERSION_ID);

	const script = document.createElement('script');
	script.async = true;
	script.src = `https://www.googletagmanager.com/gtag/js?id=${CONVERSION_ID}`;
	document.head.appendChild(script);
}

export function initGoogleAdsIfConsented() {
	if (getConsent() === 'granted') loadGoogleAds();
}

// label is a key into CONVERSION_LABELS — e.g. trackConversion('githubClick').
export function trackConversion(label) {
	if (getConsent() !== 'granted' || !CONVERSION_ID) return;
	const conversionLabel = CONVERSION_LABELS[label];
	if (!conversionLabel) return;
	loadGoogleAds();
	if (typeof window.gtag !== 'function') return;
	window.gtag('event', 'conversion', { send_to: `${CONVERSION_ID}/${conversionLabel}` });
}

// Wires up the three link-click conversions site-wide. Safe to call on
// every page — querySelectorAll just finds nothing on pages without
// these links.
export function wireLinkConversions() {
	document.querySelectorAll('a[href*="github.com/hupi-dev"]').forEach((el) => {
		el.addEventListener('click', () => trackConversion('githubClick'), { once: false });
	});
	document.querySelectorAll('a[href*="marketplace.visualstudio.com"]').forEach((el) => {
		el.addEventListener('click', () => trackConversion('marketplaceClick'), { once: false });
	});
	document.querySelectorAll('a[href^="mailto:"]').forEach((el) => {
		el.addEventListener('click', () => trackConversion('contactClick'), { once: false });
	});
}

// 30-second-dwell engagement conversion for /docs and /faq — a weaker
// proxy signal for pages with no click-through action of their own.
export function wireEngagementConversion() {
	if (!['/docs', '/docs/', '/faq', '/faq/'].includes(window.location.pathname)) return;
	setTimeout(() => trackConversion('docsEngagement'), 30_000);
}
