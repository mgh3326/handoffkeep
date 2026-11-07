package store

import "testing"

func TestDotsHKRequestIsNotLinearEligible(t *testing.T) {
	task := Task{Source: "dots", SourceRef: "hk-test", Priority: 1, Refs: TaskRefs{Linear: &TaskLinear{Sync: true}, DecisionRequest: &DecisionRequest{Status: DecisionRequestOpen}}}
	if got := LinearMirrorReason(task); got != "" {
		t.Fatalf("hk dots request admitted to Linear: %q", got)
	}
}

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
	if _, err := s.CreateDotsRequest(t.Context(), "caller", in); err == nil {
		t.Fatal("different request with the same key was accepted")
	}
}
