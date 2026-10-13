package redact

import (
	"strings"
	"testing"
)

func TestSecrets(t *testing.T) {
	cases := map[string]string{
		"Authorization: Bearer abc.def-123":                                    "Authorization: Bearer [REDACTED]",
		`db_password="hunter22"`:                                               `db_password="[REDACTED]`,
		"API_KEY=sk-ant-REDACTEDREDACTEDREDACTED":                              "API_KEY=[REDACTED]",
		"url postgres://user:s3cret@db:5432/x":                                 "url postgres://user:[REDACTED]@db:5432/x",
		"key AKIAABCDEFGHIJKLMNOP":                                             "key [REDACTED AWS KEY]",
		"-----BEGIN RSA PRIVATE KEY-----\nMIIE\n-----END RSA PRIVATE KEY-----": "[REDACTED PRIVATE KEY]",
	}
	for in, want := range cases {
		if got := Secrets(in); !strings.HasPrefix(got, strings.TrimSuffix(want, `"`)) {
			t.Errorf("Secrets(%q) = %q, want %q", in, got, want)
		}
	}
	if got := Secrets("restarts=3 status=Running"); got != "restarts=3 status=Running" {
		t.Errorf("unrelated text changed: %q", got)
	}
}

func TestTruncate(t *testing.T) {
	s := strings.Repeat("line\n", 100)
	out := Truncate(s, 52)
	if !strings.Contains(out, "truncated") || len(out) > 120 {
		t.Fatalf("bad truncation: %q", out)
	}
	if Truncate("short", 100) != "short" {
		t.Fatal("short text changed")
	}
}
