package store

import (
	"encoding/json"
	"testing"
)

func TestCheckJobOrigin(t *testing.T) {
	ok := []string{
		"https://github.com/owner/repo.git",
		"HTTPS://github.com/owner/repo.git",
		"ssh://github.com/owner/repo.git",
		"git@github.com:owner/repo.git",
		"origin",
		"repo",
	}
	for _, o := range ok {
		if err := checkJobOrigin(o); err != nil {
			t.Fatalf("%s: unexpected %v", o, err)
		}
	}
	bad := []string{
		"https://user:tok@github.com/owner/repo.git",
		"HTTPS://user:tok@github.com/owner/repo.git",
		"http://user:pw@example.com/repo",
		"ssh://user:pass@host/repo",
		"git://u:p@host/x",
		"ssh://git@host/owner/repo.git",
		"tok@github.com:owner/repo.git",
		"user:hunter2@github.com:owner/repo",
		"git@github.com:owner@x/repo",
		"file:///srv/repo",
		"/srv/repo",
		"~/repo",
		"./repo",
		"..",
		"repo.git",
		`C:\repo`,
	}
	for _, o := range bad {
		if err := checkJobOrigin(o); err == nil {
			t.Fatalf("%s: accepted", o)
		}
	}
}

func TestCheckJobUpdateResultEscapes(t *testing.T) {
	done := "completed"
	for name, raw := range map[string]string{
		"lone surrogate": `{"s":"\ud800"}`,
		"NUL":            `{"s":"\u0000"}`,
		"surrogate key":  `{"\ud800":1}`,
	} {
		if err := checkJobUpdate(JobUpdate{State: &done, Result: json.RawMessage(raw)}); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
	if err := checkJobUpdate(JobUpdate{State: &done, Result: json.RawMessage(`{"findings":[]}`)}); err != nil {
		t.Fatalf("plain result: %v", err)
	}
}

func TestCheckJobUpdateParentJobScanned(t *testing.T) {
	p := "AKIAIOSFODNN7EXAMPLE"
	if !ValidJobID(p) {
		t.Skip("job-id alphabet does not admit the sample")
	}
	if err := checkJobUpdate(JobUpdate{ParentJob: &p}); err == nil {
		t.Fatal("secret-shaped parent_job accepted")
	}
}
