// Package redact removes secrets from tool output and bounds its size before
// it reaches an LLM.
package redact

import (
	"fmt"
	"regexp"
	"strings"
)

var patterns = []struct {
	re   *regexp.Regexp
	repl string
}{
	{regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`), "[REDACTED PRIVATE KEY]"},
	{regexp.MustCompile(`(?i)(authorization\s*[:=]\s*)(bearer|basic)\s+[A-Za-z0-9._~+/=-]+`), "${1}${2} [REDACTED]"},
	{regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`), "[REDACTED JWT]"},
	{regexp.MustCompile(`\b(AKIA|ASIA)[A-Z0-9]{16}\b`), "[REDACTED AWS KEY]"},
	{regexp.MustCompile(`\b(sk-ant-|sk-|ghp_|gho_|glpat-|xox[abp]-|hf_)[A-Za-z0-9_-]{16,}`), "[REDACTED TOKEN]"},
	{regexp.MustCompile(`(?i)\b([a-z0-9_.-]*(password|passwd|secret|token|api[_-]?key)[a-z0-9_.-]*)(["']?\s*[:=]\s*["']?)([^\s"',}]{4,})`), "${1}${3}[REDACTED]"},
	{regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://[^/\s:@]+:)([^@\s/]+)(@)`), "${1}[REDACTED]${3}"},
}

// Secrets replaces credentials that commonly leak into logs and configs.
func Secrets(s string) string {
	for _, p := range patterns {
		s = p.re.ReplaceAllString(s, p.repl)
	}
	return s
}

// Truncate keeps at most max bytes, cutting at a line boundary, and says so.
func Truncate(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	cut := strings.LastIndex(s[:max], "\n")
	if cut < max/2 {
		cut = max
	}
	return s[:cut] + fmt.Sprintf("\n... [truncated: %d of %d bytes shown; narrow the query]", cut, len(s))
}
