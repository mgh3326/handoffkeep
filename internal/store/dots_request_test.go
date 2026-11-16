package store

import (
	"errors"
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/guard"
)

func TestCreateDotsRequest(t *testing.T) {
	s := schemaStore(t, "dots_request")
	ctx := t.Context()
	first, err := s.CreateDotsRequest(ctx, "caller", DotsRequestInput{Title: "Research", Body: "Please investigate", RequesterLane: "requester"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.CreateDotsRequest(ctx, "caller", DotsRequestInput{Title: "Research", Body: "Please investigate"})
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID || first.Lane != "dots" || first.ParentLane != "requester" || first.CreatedBy != "caller" || first.State != StateBacklog || first.Source != "dots" || first.SourceRef == "" || second.ParentLane != "caller" {
		t.Fatalf("first=%+v second=%+v", first, second)
	}
	doc, found, err := s.GetDocument(ctx, first.BodyDoc)
	if err != nil || !found || doc.Body != "Please investigate" {
		t.Fatalf("document=%+v found=%v err=%v", doc, found, err)
	}
	page, err := s.DotsInbox(ctx, 0, 1)
	if err != nil || len(page) != 1 || page[0].ID != first.ID {
		t.Fatalf("first page=%+v err=%v", page, err)
	}
	page, err = s.DotsInbox(ctx, first.ID, 1)
	if err != nil || len(page) != 1 || page[0].ID != second.ID {
		t.Fatalf("second page=%+v err=%v", page, err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE tasks SET lane='other' WHERE id=$1`, first.ID); err != nil {
		t.Fatal(err)
	}
	page, err = s.DotsInbox(ctx, first.ID, 1)
	if err != nil || len(page) != 1 || page[0].ID != second.ID {
		t.Fatalf("page after cursor relane=%+v err=%v", page, err)
	}
	if _, _, err := s.IntakeTask(ctx, IntakeInput{Source: "dots", SourceRef: "other-producer", Title: "Other", Lane: "dots", CreatedBy: "caller"}); err != nil {
		t.Fatal(err)
	}
	page, err = s.DotsInbox(ctx, 0, 10)
	if err != nil || len(page) != 1 || page[0].ID != second.ID {
		t.Fatalf("inbox contains non-hk intake=%+v err=%v", page, err)
	}
}

func TestCreateDotsRequestIdempotency(t *testing.T) {
	s := schemaStore(t, "dots_request_retry")
	in := DotsRequestInput{Title: "Research", Body: "Investigate", IdempotencyKey: "request-1"}
	first, err := s.CreateDotsRequest(t.Context(), "caller", in)
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.CreateDotsRequest(t.Context(), "caller", in)
	if err != nil || again.ID != first.ID {
		t.Fatalf("first=%+v again=%+v err=%v", first, again, err)
	}
	in.Body = "Different"
	if _, err := s.CreateDotsRequest(t.Context(), "caller", in); !errors.Is(err, ErrDotsRequestInvalid) || !strings.Contains(err.Error(), "already used with different input") {
		t.Fatalf("reused key err=%v", err)
	}
}

// Invariant (operator decision 346): every refusal of a dots request the
// caller can fix wraps ErrDotsRequestInvalid with a store-authored reason that
// names the failed rule, and never echoes the value it refused.
func TestCreateDotsRequestRefusalsNameTheReason(t *testing.T) {
	s := schemaStore(t, "dots_request_reason")
	template := "[목적] 홈 화면\n[유지할 것] 색상\n[범위] 1화면\n[제약] 없음\n[산출물] PNG\n[완료 기준] 채택\n[입력] 커밋 9f14c82, 캡처 환경: 로컬 빌드"
	for _, tc := range []struct {
		name        string
		in          DotsRequestInput
		reason      string
		notContains string
	}{
		{"unknown type", DotsRequestInput{Title: "x", Body: template, Type: "NO_SUCH_TYPE"}, "invalid dots request type: CREATE_MOCKUP, CREATE_ASSET or REVIEW_IMPLEMENTATION", "NO_SUCH_TYPE"},
		{"missing template section", DotsRequestInput{Title: "x", Body: "just prose", Type: DotsRequestCreateMockup}, "invalid dots request: the body lacks a [목적] section with content", ""},
		{"bad notify lane", DotsRequestInput{Title: "x", Body: "Investigate\n[알릴 레인] Not A Lane", RequesterLane: "requester"}, "the [알릴 레인] line names a lane that is not [A-Za-z0-9._-]{1,128}", "Not A Lane"},
		{"bad option", DotsRequestInput{Title: "x", Body: "Investigate", Options: []DecisionOption{{Key: "Z", Label: "one"}}}, "option keys are single letters A-F", ""},
		{"empty body", DotsRequestInput{Title: "x", Body: " "}, "body must not be empty", ""},
		{"empty title", DotsRequestInput{Body: "Investigate"}, "title must be one non-empty line", ""},
		{"bad lane", DotsRequestInput{Title: "x", Body: "Investigate", RequesterLane: "Not A Lane"}, "requester lane must match [A-Za-z0-9._-]{1,128}", "Not A Lane"},
		{"bad idempotency key", DotsRequestInput{Title: "x", Body: "Investigate", IdempotencyKey: "bad\nkey"}, "idempotency key must be 1-512 bytes without control characters", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.CreateDotsRequest(t.Context(), "caller", tc.in)
			if !errors.Is(err, ErrDotsRequestInvalid) {
				t.Fatalf("err=%v, want ErrDotsRequestInvalid", err)
			}
			reason, ok := strings.CutPrefix(err.Error(), ErrDotsRequestInvalid.Error()+": ")
			if !ok || reason != tc.reason {
				t.Fatalf("err=%q, want reason %q", err, tc.reason)
			}
			if tc.notContains != "" && strings.Contains(reason, tc.notContains) {
				t.Fatalf("reason echoes the refused input: %q", reason)
			}
		})
	}
	// A secret-like title keeps guard's own refusal, not the dots one.
	if _, err := s.CreateDotsRequest(t.Context(), "caller", DotsRequestInput{Title: "AKIA" + "ABCDEFGHIJKLMNOP", Body: "Investigate"}); errors.Is(err, ErrDotsRequestInvalid) {
		t.Fatalf("secret refusal became a dots refusal: %v", err)
	} else if _, ok := guard.SecretPattern(err); !ok {
		t.Fatalf("secret refusal lost its pattern: %v", err)
	}
}
