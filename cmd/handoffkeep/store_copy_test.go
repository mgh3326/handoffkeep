package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
)

func TestStoreCopyRejectsBlankTableSelection(t *testing.T) {
	var out bytes.Buffer
	err := storeCopyCmd([]string{
		"--from", "postgres://copy@localhost/handoffkeep",
		"--to", "mysql://copy@localhost/handoffkeep",
		"--tables", ",,",
	}, &out)
	if err == nil || !strings.Contains(err.Error(), "no tables selected") {
		t.Fatalf("storeCopyCmd error=%v, want no tables selected", err)
	}
	if out.Len() != 0 {
		t.Fatalf("storeCopyCmd wrote success output for an empty selection: %q", out.String())
	}
}

func TestStoreCopyFailurePrintsPartialReport(t *testing.T) {
	wantErr := errors.New("later table failed")
	report := store.CopyReport{Tables: []store.CopyTable{{Name: "quota_observations", SourceRows: 1, TargetRows: 1}}}
	var out bytes.Buffer
	if gotErr := storeCopyFailure(&out, report, wantErr); gotErr != wantErr {
		t.Fatalf("storeCopyFailure error=%v, want original error %v", gotErr, wantErr)
	}
	var got store.CopyReport
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("partial report is not JSON: %v", err)
	}
	if len(got.Tables) != 1 || got.Tables[0].Name != "quota_observations" {
		t.Fatalf("partial report tables=%+v, want committed quota_observations table", got.Tables)
	}
}

func TestStoreCopyURLFlagAndEnvForms(t *testing.T) {
	const secret = "postgres://copy:s3cret-pass@db.example.test:5432/handoffkeep"
	t.Setenv("HK_TEST_COPY_SRC", secret)
	t.Setenv("HK_TEST_COPY_EMPTY", "")
	cases := []struct {
		name    string
		value   string
		envName string
		want    string
		errHas  []string
	}{
		{name: "flag only", value: secret, want: secret},
		{name: "env only", envName: "HK_TEST_COPY_SRC", want: secret},
		{name: "both set", value: secret, envName: "HK_TEST_COPY_SRC", errHas: []string{"--from and --from-env are mutually exclusive"}},
		{name: "neither set", errHas: []string{"--from or --from-env is required"}},
		{name: "unset variable", envName: "HK_TEST_COPY_UNSET", errHas: []string{"HK_TEST_COPY_UNSET", "unset or empty"}},
		{name: "empty variable", envName: "HK_TEST_COPY_EMPTY", errHas: []string{"HK_TEST_COPY_EMPTY", "unset or empty"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := storeCopyURL("from", tc.value, tc.envName)
			if len(tc.errHas) == 0 {
				if err != nil || got != tc.want {
					t.Fatalf("storeCopyURL()=%q, %v; want %q", got, err, tc.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("storeCopyURL() returned %q, want an error", got)
			}
			for _, sub := range tc.errHas {
				if !strings.Contains(err.Error(), sub) {
					t.Fatalf("error %q does not contain %q", err, sub)
				}
			}
			if strings.Contains(err.Error(), "s3cret-pass") {
				t.Fatalf("error leaks the URL password: %q", err)
			}
		})
	}
}

func TestStoreCopyCommandRefusesBadURLSelection(t *testing.T) {
	t.Setenv("HK_TEST_COPY_EMPTY", "")
	cases := map[string][]string{
		"both forms":    {"--from", "postgres://u:p@h/db", "--from-env", "HK_TEST_COPY_EMPTY", "--to", "mysql://u:p@h/db"},
		"neither form":  {"--to", "mysql://u:p@h/db"},
		"empty env":     {"--from-env", "HK_TEST_COPY_EMPTY", "--to", "mysql://u:p@h/db"},
		"stray URL arg": {"postgres://copy:s3cret-pass@h/db"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			err := storeCopyCmd(args, &out)
			if err == nil {
				t.Fatalf("storeCopyCmd(%v) succeeded", args)
			}
			if strings.Contains(err.Error(), "s3cret-pass") {
				t.Fatalf("error leaks the URL password: %q", err)
			}
			if out.Len() != 0 {
				t.Fatalf("wrote output on refusal: %q", out.String())
			}
		})
	}
}

func TestStoreCopyErrorsRedactURLPasswords(t *testing.T) {
	const raw = "mysql://copy:s3cret-pass@db.example.test:3306/handoffkeep"
	echoed := errors.New(`ping database: dial "mysql://copy:s3cret-pass@db.example.test:3306/handoffkeep" failed for copy:s3cret-pass@db.example.test`)
	got := redactStoreCopyError(echoed, []string{raw})
	if got == nil || strings.Contains(got.Error(), "s3cret-pass") {
		t.Fatalf("redacted error=%v, want no password", got)
	}
	if !strings.Contains(got.Error(), "***@db.example.test") {
		t.Fatalf("redacted error=%q, want userinfo masked and host kept", got)
	}
	unknown := errors.New(`cannot parse postgres://other:hunter2@h/db`)
	if got := redactStoreCopyError(unknown, nil); strings.Contains(got.Error(), "hunter2") {
		t.Fatalf("URL not in the command still leaked: %q", got)
	}
	mismatch := exitCodeError{code: storeCopyExitMismatch, err: errors.New("parity mismatch")}
	var exit exitCodeError
	if err := redactStoreCopyError(mismatch, []string{raw}); !errors.As(err, &exit) || exit.code != storeCopyExitMismatch {
		t.Fatalf("redaction dropped the exit code: %v", err)
	}
}
