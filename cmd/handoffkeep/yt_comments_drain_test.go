package main

import (
	"errors"
	"io"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

func TestYTCommentsDrainClassifiesCurrentCommentOps(t *testing.T) {
	binding := store.YTTaskBinding{Tenant: "yt.example", IssueID: "issue-1"}
	tests := []struct {
		name      string
		op        ytwrite.Op
		binding   store.YTTaskBinding
		found     bool
		modeOn    bool
		authority store.YTAuthoritySide
		want      string
	}{
		{name: "ready", op: ytwrite.Op{State: ytwrite.StatePrepared, YTTargetID: "issue-1"}, binding: binding, found: true, modeOn: true, want: "ready"},
		{name: "dispatched", op: ytwrite.Op{State: ytwrite.StateDispatched}, modeOn: true, want: "ready"},
		{name: "held mode", op: ytwrite.Op{State: ytwrite.StatePrepared}, want: "held_mode"},
		{name: "missing binding", op: ytwrite.Op{State: ytwrite.StatePrepared, YTTargetID: "issue-1"}, modeOn: true, want: "held_binding"},
		{name: "changed binding", op: ytwrite.Op{State: ytwrite.StatePrepared, YTTargetID: "issue-2"}, binding: binding, found: true, modeOn: true, want: "held_binding_changed"},
		{name: "other tenant", op: ytwrite.Op{State: ytwrite.StatePrepared, YTTargetID: "issue-1"}, binding: store.YTTaskBinding{Tenant: "other", IssueID: "issue-1"}, found: true, modeOn: true, want: "held_other_tenant"},
		{name: "authority holds", op: ytwrite.Op{State: ytwrite.StatePrepared, YTTargetID: "issue-1"}, authority: store.YTAuthorityYouTrack, modeOn: true, want: "held_authority_youtrack"},
		{name: "unknown", op: ytwrite.Op{State: ytwrite.StateRemoteUnknown}, want: "remote_unknown"},
		{name: "targetless comment", op: ytwrite.Op{Kind: ytwrite.KindCommentAppend, State: ytwrite.StatePrepared}, modeOn: true, want: "ready"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ytBindingDrainClass(tt.op, tt.binding, tt.found, "yt.example", tt.modeOn, tt.authority); got != tt.want {
				t.Fatalf("class = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestYTCommentsDrainRejectHeldRequiresModeOn(t *testing.T) {
	t.Setenv("HANDOFFKEEP_YT_COMMENTS_MODE", "off")
	t.Setenv("HANDOFFKEEP_DB_URL", "")
	var code exitCodeError
	err := run([]string{"yt", "comments-drain", "--reject-held"}, io.Discard, io.Discard)
	if !errors.As(err, &code) || code.code != youtrackExitConfig {
		t.Fatalf("comments-drain --reject-held = %v, want exit %d", err, youtrackExitConfig)
	}
}
