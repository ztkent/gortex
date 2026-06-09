package review

import (
	"strings"
	"testing"
)

func TestRedactSecrets_RemovesPlantedSecrets(t *testing.T) {
	cases := []struct {
		name   string
		secret string
		body   string
	}{
		{
			name:   "aws access key id",
			secret: "AKIA0123456789ABCDEF",
			body:   "This line leaks a credential: AKIA0123456789ABCDEF — move it to an env var.",
		},
		{
			name:   "aws secret assignment",
			secret: "wJalrXUtnFEMI0K7MDENGbPxRfiCYz4hk9Qm3n8p",
			body:   "aws_secret_key = wJalrXUtnFEMI0K7MDENGbPxRfiCYz4hk9Qm3n8p",
		},
		{
			name:   "github pat",
			secret: "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789",
			body:   "token leaked in the diff: ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789",
		},
		{
			name:   "openai key",
			secret: "sk-proj-abcdef0123456789ABCDEFXYZ",
			body:   "client = OpenAI(api_key=\"sk-proj-abcdef0123456789ABCDEFXYZ\")",
		},
		{
			name:   "password assignment",
			secret: "hunter2hunter2hunter",
			body:   "password = \"hunter2hunter2hunter\" should not be hard-coded.",
		},
		{
			name:   "bearer token",
			secret: "abcdef0123456789ABCDEF.token",
			body:   "Authorization: Bearer abcdef0123456789ABCDEF.token",
		},
		{
			name:   "pem private key block",
			secret: "MIIEpAIBAAKCAQEAabcdef",
			body: "Key embedded in body:\n" +
				"-----BEGIN RSA PRIVATE KEY-----\n" +
				"MIIEpAIBAAKCAQEAabcdef\n" +
				"-----END RSA PRIVATE KEY-----\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clean, hits := RedactSecrets(tc.body)
			if hits == 0 {
				t.Fatalf("expected at least one redaction, got 0 (body=%q)", tc.body)
			}
			if strings.Contains(clean, tc.secret) {
				t.Fatalf("secret %q still present after redaction: %q", tc.secret, clean)
			}
			if !strings.Contains(clean, redactPlaceholder) {
				t.Fatalf("expected placeholder %q in redacted body, got %q", redactPlaceholder, clean)
			}
		})
	}
}

func TestRedactSecrets_LeavesPlaceholdersAndCleanBody(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"empty", ""},
		{"plain prose", "This function returns nil without checking the error."},
		{"placeholder password", `password = "changeme"`},
		{"example token", "set token=your-token-here in the env"},
		{"todo secret", "secret = TODO_set_me_later"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clean, hits := RedactSecrets(tc.body)
			if hits != 0 {
				t.Fatalf("expected 0 redactions for clean body, got %d (clean=%q)", hits, clean)
			}
			if clean != tc.body {
				t.Fatalf("clean body was mutated: %q -> %q", tc.body, clean)
			}
		})
	}
}

func TestRedactSecrets_KeepsKeyFramingReadable(t *testing.T) {
	clean, hits := RedactSecrets(`api_key = "AKIA0123456789ABCDEF"`)
	if hits == 0 {
		t.Fatal("expected redaction")
	}
	if strings.Contains(clean, "AKIA0123456789ABCDEF") {
		t.Fatalf("secret still present: %q", clean)
	}
	// The key name stays readable so the reviewer knows what was redacted.
	if !strings.Contains(clean, "api_key") {
		t.Fatalf("expected key name to survive, got %q", clean)
	}
}
