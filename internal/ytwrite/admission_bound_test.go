package ytwrite

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// Admission refuses what rendering would refuse. A comment or issue whose body
// fits the bound alone, but not with its provenance line and marker, is refused
// before anything is stored; a comment that is admitted renders at every op id.
func TestAdmissionMatchesTheRenderedBound(t *testing.T) {
	commentReq := func(body string) CommentRequest {
		return CommentRequest{Principal: "p", Actor: "worker", TaskID: 1, YTIssueID: "2-1", Tenant: "t", Body: body, Key: "k"}
	}

	// Within the body bound, over the sent-text bound: refused.
	near := strings.Repeat("x", TextMaxChars-50)
	if err := CheckComment(commentReq(near)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("CheckComment admitted a comment whose sent text is over the bound: %v", err)
	}
	if err := CheckCommentContent("worker", near); !errors.Is(err, ErrInvalid) {
		t.Fatalf("CheckCommentContent admitted a comment whose sent text is over the bound: %v", err)
	}

	// Admitted: it renders at the smallest and the largest op id.
	body := strings.Repeat("x", TextMaxChars-1000)
	if err := CheckComment(commentReq(body)); err != nil {
		t.Fatalf("CheckComment refused a comment that fits: %v", err)
	}
	payload, err := json.Marshal(CommentPayload{Body: body})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{1, worstOpID} {
		op := Op{ID: id, Actor: "worker", CreatedAt: time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC), Payload: payload}
		if _, err := RenderComment(op); err != nil {
			t.Fatalf("an admitted comment is refused by render at op %d: %v", id, err)
		}
	}

	// Issues: the title alone is within the bound, the description is not.
	issueReq := IssueRequest{Principal: "p", Actor: "worker", TaskID: 1, Title: strings.Repeat("x", TextMaxChars-50), Project: "handoffkeep", YTProjectID: "0-1", YTProjectShort: "HK", Tenant: "t"}
	if err := CheckIssue(issueReq); !errors.Is(err, ErrInvalid) {
		t.Fatalf("CheckIssue admitted an issue whose description is over the bound: %v", err)
	}
	issueReq.Title = strings.Repeat("x", TextMaxChars-1000)
	if err := CheckIssue(issueReq); err != nil {
		t.Fatalf("CheckIssue refused an issue that fits: %v", err)
	}
}
