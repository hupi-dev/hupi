package mcpserver

import "regexp"

// secretPatterns catch credentials a model might paste into a write tool.
// Memory is long-lived, encrypted at rest but decrypted into every later
// prompt that retrieves it, so a token stored once would keep resurfacing.
// This is a guard against obvious mistakes, not a DLP system: it only
// matches well-known, high-confidence formats.
var secretPatterns = []struct {
	kind string
	re   *regexp.Regexp
}{
	{"HUPI API key", regexp.MustCompile(`hupi_sk_[A-Za-z0-9_-]{16,}`)},
	{"OpenAI/Anthropic API key", regexp.MustCompile(`\bsk-(?:proj-|ant-|svcacct-)?[A-Za-z0-9_-]{20,}`)},
	{"Stripe secret key", regexp.MustCompile(`\b[rs]k_live_[A-Za-z0-9]{20,}`)},
	{"AWS access key id", regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{"GitHub token", regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{22,})`)},
	{"Slack token", regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9-]{10,}`)},
	{"Google API key", regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}`)},
	{"private key", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
	{"JSON Web Token", regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`)},
}

// findSecret returns the kind of the first credential found in texts, or "".
func findSecret(texts ...string) string {
	for _, t := range texts {
		for _, p := range secretPatterns {
			if p.re.MatchString(t) {
				return p.kind
			}
		}
	}
	return ""
}
