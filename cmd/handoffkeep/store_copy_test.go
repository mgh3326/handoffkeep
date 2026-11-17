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
