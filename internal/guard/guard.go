// Package guard rejects credential-shaped content before it reaches storage.
package guard

import (
	"fmt"
	"regexp"
)

var patterns = []struct {
	name string
	re   *regexp.Regexp
}{
	// OpenAI API-key prefixes are lowercase. Keeping the match case-sensitive
	// avoids treating ordinary uppercase SK-* identifiers as credentials.
	{"openai_key", regexp.MustCompile(`\bsk-(?:proj-|svcacct-|admin-)?[A-Za-z0-9]{20,}\b`)},
	{"anthropic_key", regexp.MustCompile(`\bsk-ant-(?:api03-)?[A-Za-z0-9]{20,}\b`)},
	{"github_token", regexp.MustCompile(`\bghp_[A-Za-z0-9]{36}\b`)},
	{"slack_token", regexp.MustCompile(`\bxoxb-[0-9]{10,13}-[0-9]{10,13}-[A-Za-z0-9]{20,}\b`)},
	{"aws_access_key", regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`)},
	{"private_key", regexp.MustCompile(`-----BEGIN`)},
	{"bearer_token", regexp.MustCompile(`(?i)\bBearer [A-Za-z0-9._-]{20,}`)},
	{"token_assignment", regexp.MustCompile(`(?m)\b[A-Z_]*(?:TOKEN|SECRET|KEY)=[^\s]{12,}`)},
	{"github_fine_grained_token", regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{82}\b`)},
}

// Match returns only a stable pattern name; it never returns secret content.
func Match(value string) string {
	for _, p := range patterns {
		if p.re.MatchString(value) {
			return p.name
		}
	}
	return ""
}
func Reject(value string) error {
	if p := Match(value); p != "" {
		return fmt.Errorf("secret_like_content:%s", p)
	}
	return nil
}
