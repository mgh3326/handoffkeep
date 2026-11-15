package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// hk 4788: a decision option carries the pictures and videos the operator
// compares it by, and the request carries the recommendation's reason, what
// was not verified and one fix.

func decisionMediaStore(t *testing.T) *Store {
	t.Helper()
	return schemaStore(t, "decision_media")
}

func mediaAttachment(t *testing.T, s *Store, n int, mime, name string) string {
	t.Helper()
	sha := fmt.Sprintf("%064x", n)
	x := Attachment{SHA256: sha, SizeBytes: 3, MIME: mime, OriginalName: name, CreatedBy: "dr-test", RefKind: "task", RefID: "1"}
	if _, _, err := s.PutAttachment(context.Background(), x, 1<<20, 100); err != nil {
		t.Fatal(err)
	}
	return sha
}

func mediaInput(shas ...string) DecisionRequestInput {
	in := decisionInput()
	in.Options.Options = []DecisionOption{
		{Key: "A", Label: "2A left at the signal"},
		{Key: "B", Label: "2B left at the exit", Recommended: true},
	}
	for i, sha := range shas {
		in.Options.Options[i%2].Attachments = append(in.Options.Options[i%2].Attachments, DecisionOptionAttachment{
			SHA256:  sha,
			Caption: fmt.Sprintf("caption %d", i),
		})
	}
	in.RecommendationReason = "2B keeps the left line where 2A already had it.\nNothing new on 4B."
	in.Unverified = []string{"head relation of the Stage 2/3 package", "checker figures"}
	in.OneFix = "move the sign 20 m earlier"
	return in
}

// The recorded request reads back with every attachment in the order the
// producer gave it, and with the three new request fields.
func TestDecisionRequestRecordsOptionMediaInOrder(t *testing.T) {
	s := decisionMediaStore(t)
	ctx := context.Background()
	first := mediaAttachment(t, s, 1, "image/png", "approach.png")
	second := mediaAttachment(t, s, 2, "video/mp4", "key.mp4")
	third := mediaAttachment(t, s, 3, "image/png", "end.png")
	task := decisionTask(t, s, "in_progress")
	in := mediaInput(first, second, third)
	got, err := s.RecordDecisionRequest(ctx, task.ID, "director-1", in)
	if err != nil {
		t.Fatal(err)
	}
	read, found, err := s.GetTask(ctx, task.ID)
	if err != nil || !found {
		t.Fatalf("read back: found=%v err=%v", found, err)
	}
	options := read.Refs.DecisionOptions
	if options == nil || len(options.Options) != 2 {
		t.Fatalf("options=%+v", options)
	}
	wantA := []DecisionOptionAttachment{{SHA256: first, Caption: "caption 0"}, {SHA256: third, Caption: "caption 2"}}
	if fmt.Sprint(options.Options[0].Attachments) != fmt.Sprint(wantA) {
		t.Fatalf("option A attachments=%+v want %+v", options.Options[0].Attachments, wantA)
	}
	if len(options.Options[1].Attachments) != 1 || options.Options[1].Attachments[0].SHA256 != second {
		t.Fatalf("option B attachments=%+v", options.Options[1].Attachments)
	}
	request := read.Refs.DecisionRequest
	if request == nil || request.RecommendationReason != in.RecommendationReason ||
		strings.Join(request.Unverified, "|") != strings.Join(in.Unverified, "|") || request.OneFix != in.OneFix {
		t.Fatalf("request=%+v", request)
	}
	if got.Request.RecommendationReason != in.RecommendationReason {
		t.Fatalf("returned request=%+v", got.Request)
	}
	// The same command again is a duplicate: nothing new is written, and the
	// media survives the duplicate check.
	again, err := s.RecordDecisionRequest(ctx, task.ID, "director-1", in)
	if err != nil || !again.Duplicate || again.Request.ID != got.Request.ID {
		t.Fatalf("re-send: duplicate=%v id=%q err=%v", again.Duplicate, again.Request.ID, err)
	}
}

// An option naming a content address no attachment row holds is refused with
// the sentinel the API maps to decision_attachment_not_found, and nothing is
// recorded.
func TestDecisionOptionAttachmentMustExist(t *testing.T) {
	s := decisionMediaStore(t)
	ctx := context.Background()
	task := decisionTask(t, s, "in_progress")
	unknown := strings.Repeat("e", 64)
	_, err := s.RecordDecisionRequest(ctx, task.ID, "director-1", mediaInput(unknown))
	if !errors.Is(err, ErrDecisionAttachmentNotFound) {
		t.Fatalf("err=%v", err)
	}
	read, _, err := s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if read.Refs.DecisionRequest != nil || read.Refs.DecisionOptions != nil {
		t.Fatalf("a refused request was written: %+v %+v", read.Refs.DecisionRequest, read.Refs.DecisionOptions)
	}
}

// Every cap is enforced before anything is written.
func TestDecisionRequestMediaCaps(t *testing.T) {
	s := decisionMediaStore(t)
	ctx := context.Background()
	task := decisionTask(t, s, "in_progress")
	shas := make([]string, 9)
	for i := range shas {
		shas[i] = mediaAttachment(t, s, i+10, "image/png", "x.png")
	}
	cases := []struct {
		name string
		in   func() DecisionRequestInput
		want string
	}{
		{"nine attachments", func() DecisionRequestInput {
			in := decisionInput()
			in.Options.Options = []DecisionOption{{Key: "A", Label: "x"}}
			for _, sha := range shas {
				in.Options.Options[0].Attachments = append(in.Options.Options[0].Attachments, DecisionOptionAttachment{SHA256: sha})
			}
			return in
		}, "at most 8 are allowed"},
		{"caption over the bound", func() DecisionRequestInput {
			in := decisionInput()
			in.Options.Options = []DecisionOption{{Key: "A", Label: "x", Attachments: []DecisionOptionAttachment{{SHA256: shas[0], Caption: strings.Repeat("가", DecisionOptionCaptionMaxBytes)}}}}
			return in
		}, "the limit is 200 bytes"},
		{"same address twice", func() DecisionRequestInput {
			in := decisionInput()
			in.Options.Options = []DecisionOption{{Key: "A", Label: "x", Attachments: []DecisionOptionAttachment{{SHA256: shas[0]}, {SHA256: shas[0]}}}}
			return in
		}, "twice"},
		{"not a content address", func() DecisionRequestInput {
			in := decisionInput()
			in.Options.Options = []DecisionOption{{Key: "A", Label: "x", Attachments: []DecisionOptionAttachment{{SHA256: "not-a-sha"}}}}
			return in
		}, "64 lower-case hex"},
		{"eleven unverified items", func() DecisionRequestInput {
			in := decisionInput()
			for i := 0; i < DecisionRequestMaxUnverified+1; i++ {
				in.Unverified = append(in.Unverified, fmt.Sprintf("item %d", i))
			}
			return in
		}, "at most 10 unverified items"},
		{"unverified item over the bound", func() DecisionRequestInput {
			in := decisionInput()
			in.Unverified = []string{strings.Repeat("x", DecisionRequestUnverifiedMaxBytes+1)}
			return in
		}, "each unverified item"},
		{"one fix over the bound", func() DecisionRequestInput {
			in := decisionInput()
			in.OneFix = strings.Repeat("x", DecisionRequestOneFixMaxBytes+1)
			return in
		}, "one_fix must be at most"},
		{"recommendation reason over the bound", func() DecisionRequestInput {
			in := decisionInput()
			in.RecommendationReason = strings.Repeat("x", DecisionRequestRecommendationReasonMaxBytes+1)
			return in
		}, "recommendation_reason must be at most"},
	}
	for _, c := range cases {
		_, err := s.RecordDecisionRequest(ctx, task.ID, "director-1", c.in())
		if !errors.Is(err, ErrInvalidDecisionRequest) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err=%v want %q", c.name, err, c.want)
		}
	}
}

// A row written before options could carry media reads back with empty
// lists: the stored JSON has no attachments key at all.
func TestDecisionOptionsWithoutMediaReadAsEmpty(t *testing.T) {
	s := decisionMediaStore(t)
	ctx := context.Background()
	x, err := s.CreateTask(ctx, Task{
		Lane: "b4788-old-shape", Title: "old shape", Kind: "implement", CreatedBy: "dr-test",
		Project: projectPtr(testProjectName),
	})
	if err != nil {
		t.Fatal(err)
	}
	// A row written before options could carry media holds the two refs keys
	// with no attachments field at all. CreateTask refuses decision refs (they
	// are written by tasks decision-request only), so the old row is written
	// directly, the way a previous release left it.
	legacy, err := json.Marshal(TaskRefs{
		DecisionOptions: &DecisionOptions{AllowFree: true, Options: []DecisionOption{{Key: "A", Label: "old A"}, {Key: "B", Label: "old B", Recommended: true}}},
		DecisionRequest: &DecisionRequest{ID: "dr-1-1", Revision: 1, Root: "dr-1-1", Status: DecisionRequestOpen, Question: "old?", DefaultAction: "none", RequestedBy: "dr-test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE tasks SET refs = COALESCE(refs, '{}'::jsonb) || $2::jsonb WHERE id=$1`, x.ID, legacy); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := s.pool.QueryRow(ctx, `SELECT refs->'decision_options'->>'options' FROM tasks WHERE id=$1`, x.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "attachments") {
		t.Fatalf("an option with no media wrote an attachments key: %s", raw)
	}
	read, found, err := s.GetTask(ctx, x.ID)
	if err != nil || !found {
		t.Fatalf("read back: found=%v err=%v", found, err)
	}
	for _, option := range read.Refs.DecisionOptions.Options {
		if len(option.Attachments) != 0 {
			t.Fatalf("option %s read attachments %+v", option.Key, option.Attachments)
		}
	}
	if read.Refs.DecisionRequest.RecommendationReason != "" || read.Refs.DecisionRequest.Unverified != nil || read.Refs.DecisionRequest.OneFix != "" {
		t.Fatalf("old request grew new fields: %+v", read.Refs.DecisionRequest)
	}
}

// The previous release decodes a row this one writes: its DecisionOption has
// key, label and recommended only, and the extra attachments key is ignored
// (blue-green: the previous release must still read what the new one writes).
func TestPreviousReleaseDecodesOptionMedia(t *testing.T) {
	type previousOption struct {
		Key         string `json:"key"`
		Label       string `json:"label"`
		Recommended bool   `json:"recommended"`
	}
	options := DecisionOptions{AllowFree: true, Options: []DecisionOption{{
		Key: "A", Label: "2A", Recommended: true,
		Attachments: []DecisionOptionAttachment{{SHA256: strings.Repeat("a", 64), Caption: "approach"}},
	}}}
	wire, err := json.Marshal(options)
	if err != nil {
		t.Fatal(err)
	}
	var previous struct {
		Options   []previousOption `json:"options"`
		AllowFree bool             `json:"allow_free"`
	}
	if err := json.Unmarshal(wire, &previous); err != nil {
		t.Fatalf("previous release refused the row: %v", err)
	}
	if len(previous.Options) != 1 || previous.Options[0].Key != "A" || previous.Options[0].Label != "2A" || !previous.Options[0].Recommended {
		t.Fatalf("previous release read %+v", previous.Options)
	}
}
