package review

import (
	"regexp"
	"strings"
)

// redactPlaceholder replaces a matched secret span in a comment body before the
// body ever leaves the machine. A reviewer can still tell a secret was present
// (the placeholder is visible) without the secret itself egressing.
const redactPlaceholder = "«redacted»"

// secretValuePatterns are value-shaped credential matchers: a secret is
// recognised by the SHAPE of the string regardless of any surrounding key name.
// These cover the high-confidence formats that a leaked credential almost always
// matches — provider-issued key/token prefixes and PEM private-key blocks — so a
// body that merely quotes the raw secret (no `key = value` framing) is still
// caught. Order does not matter; every pattern is applied.
var secretValuePatterns = []*regexp.Regexp{
	// PEM private-key block (RSA / EC / OPENSSH / generic). Matched whole so the
	// entire armored body is replaced, not just the header line.
	regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`),
	// AWS access-key id (AKIA / ASIA / AGPA / AIDA / AROA …) — 16 trailing A-Z0-9.
	regexp.MustCompile(`\b(?:AKIA|ASIA|AGPA|AIDA|AROA|AIPA|ANPA|ANVA|ABIA|ACCA)[A-Z0-9]{16}\b`),
	// GitHub personal-access / app / refresh / OAuth tokens.
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{30,}\b`),
	// GitHub fine-grained PAT.
	regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{40,}\b`),
	// OpenAI-style secret keys (sk-… / sk-proj-…).
	regexp.MustCompile(`\bsk-(?:proj-)?[A-Za-z0-9_-]{20,}\b`),
	// Slack tokens.
	regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}\b`),
	// Google API key.
	regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`),
	// JWT (three base64url segments).
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b`),
	// Bearer / Authorization token in prose.
	regexp.MustCompile(`(?i)\b(?:bearer|authorization:\s*bearer)\s+[A-Za-z0-9._~+/=-]{12,}`),
}

// secretAssignPattern catches the `name = value` / `name: value` form where the
// key name reads like a credential. The credential-name alternation is the same
// vocabulary the SAST hardcoded-secret detector keys on (password / passwd /
// secret / api_key / token / aws_secret_key / access_key / private_key), so the
// redactor and the detector agree on what "looks like a credential". The value
// run is captured separately so only the value is redacted, leaving the key
// readable in the comment.
var secretAssignPattern = regexp.MustCompile(
	`(?i)\b(password|passwd|secret|api[_-]?key|token|aws[_-]?secret(?:[_-]?key)?|access[_-]?key|private[_-]?key)\b\s*[:=]\s*['"` + "`" + `]?([^'"` + "`" + `\s,;)]{6,})['"` + "`" + `]?`,
)

// secretPlaceholderMarkers are obvious non-secret values the assignment matcher
// must NOT redact: example / fixture / placeholder strings that happen to sit
// under a credential-looking key. This is the same rejection vocabulary the SAST
// secret detectors apply (secretLiteralLooksReal), so a body quoting
// `password = changeme` is left intact rather than noisily redacted.
var secretPlaceholderMarkers = []string{
	"todo", "fixme", "changeme", "placeholder", "example", "your-", "xxx",
	"***", "...", "<", "redacted", "dummy", "sample", "test", "demo", "none", "null",
}

// looksLikePlaceholder reports whether a value is an obvious non-secret
// placeholder, so the assignment-form redactor can leave it untouched.
func looksLikePlaceholder(val string) bool {
	low := strings.ToLower(strings.TrimSpace(val))
	if low == "" {
		return true
	}
	for _, m := range secretPlaceholderMarkers {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

// RedactSecrets scrubs secrets out of a comment body before it egresses to a
// forge. It reuses the SAST secret-detector vocabulary (the same credential key
// names and placeholder-rejection markers) plus a regex denylist of value-shaped
// credentials (AWS keys, GitHub / OpenAI / Slack tokens, PEM private-key blocks,
// bearer tokens). Every matched secret span is replaced with «redacted»; the
// returned int is the number of distinct spans redacted. A body with no secret
// is returned unchanged with a zero count.
//
// This is the pre-egress gate for posted review comments: a finding body — LLM
// free text or a quoted source line — can carry an inline credential, and the
// finding's identity key already folds the flagged source text in, confirming
// source text flows into findings. RedactSecrets ensures the secret is gone
// before any payload is built or any request is sent.
func RedactSecrets(body string) (string, int) {
	if body == "" {
		return body, 0
	}
	hits := 0
	out := body

	// Value-shaped patterns first: they match the secret regardless of framing,
	// so a raw key pasted into prose is caught even with no `key = value` form.
	for _, re := range secretValuePatterns {
		out = re.ReplaceAllStringFunc(out, func(string) string {
			hits++
			return redactPlaceholder
		})
	}

	// Assignment form: redact only the value, and only when it does not look like
	// an obvious placeholder (so example / fixture strings stay readable).
	out = secretAssignPattern.ReplaceAllStringFunc(out, func(m string) string {
		sub := secretAssignPattern.FindStringSubmatch(m)
		if len(sub) < 3 || looksLikePlaceholder(sub[2]) {
			return m
		}
		hits++
		// Preserve the leading `key<sep>` framing, redact the value span.
		keyPart := m[:strings.LastIndex(m, sub[2])]
		return keyPart + redactPlaceholder
	})

	return out, hits
}
