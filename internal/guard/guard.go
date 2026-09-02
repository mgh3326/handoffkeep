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
	{"openai_key", regexp.MustCompile(`(?i)\bsk-[A-Za-z0-9_-]{8,}`)},
	{"github_token", regexp.MustCompile(`\bghp_[A-Za-z0-9]{8,}`)},
	{"slack_token", regexp.MustCompile(`\bxoxb-[A-Za-z0-9-]{8,}`)},
	{"aws_access_key", regexp.MustCompile(`\bAKIA[A-Z0-9]{8,}`)},
	{"private_key", regexp.MustCompile(`-----BEGIN`)},
	{"bearer_token", regexp.MustCompile(`(?i)\bBearer [A-Za-z0-9._-]{20,}`)},
	{"token_assignment", regexp.MustCompile(`(?m)\b[A-Z_]*(?:TOKEN|SECRET|KEY)=[^\s]{12,}`)},
	{"github_fine_grained_token", regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{8,}`)},
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
