package store

import (
	"regexp"
	"strings"
	"testing"
)

func twoOptions() *DecisionOptions {
	return &DecisionOptions{Options: []DecisionOption{{Key: "A", Label: "ship"}, {Key: "B", Label: "hold"}}}
}

func TestDecisionRequestVer(t *testing.T) {
	options := *twoOptions()
	base := DecisionRequestVer("q", options, "doc/1", []string{"r"})
	if !regexp.MustCompile(`^[0-9a-f]{8}$`).MatchString(base) {
		t.Fatalf("ver=%q", base)
	}
	if again := DecisionRequestVer("q", *twoOptions(), "doc/1", []string{"r"}); again != base {
		t.Fatalf("not stable: %q %q", base, again)
	}
	relabelled := *twoOptions()
	relabelled.Options[1].Label = "wait"
	for name, other := range map[string]string{
		"question":                      DecisionRequestVer("q2", options, "doc/1", []string{"r"}),
		"option":                        DecisionRequestVer("q", relabelled, "doc/1", []string{"r"}),
		"doc":                           DecisionRequestVer("q", options, "doc/2", []string{"r"}),
		"ref":                           DecisionRequestVer("q", options, "doc/1", []string{"r2"}),
		"text moved between q and refs": DecisionRequestVer("qr", options, "doc/1", nil),
	} {
		if other == base {
			t.Errorf("%s change kept ver %s", name, base)
		}
	}
}

func TestLinearDecisionUUID(t *testing.T) {
	id := LinearDecisionUUID("issue:dr-1-1")
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(id) {
		t.Fatalf("not a v4 shaped uuid: %q", id)
	}
	if id != LinearDecisionUUID("issue:dr-1-1") || id == LinearDecisionUUID("issue:dr-1-2") {
		t.Fatal("must be stable per key and differ across keys")
	}
}

func TestParseLinearDecisionComment(t *testing.T) {
	cases := []struct {
		body string
		ok   bool
		want LinearDecisionComment
	}{
		{"1", true, LinearDecisionComment{Choice: "1"}},
		{"  2  \nbecause it is cheaper", true, LinearDecisionComment{Choice: "2"}},
		{"A", true, LinearDecisionComment{Choice: "A"}},
		{"b", true, LinearDecisionComment{Choice: "b"}},
		{"D-2615-1 1", true, LinearDecisionComment{Ref: "D-2615-1", Choice: "1"}},
		{"d-2615-1 1", true, LinearDecisionComment{Ref: "d-2615-1", Choice: "1"}},
		{"D-2615 1", true, LinearDecisionComment{Ref: "D-2615", Choice: "1"}},
		{"D-2615-1 ab12cd3e 2", true, LinearDecisionComment{Ref: "D-2615-1", Ver: "ab12cd3e", Choice: "2"}},
		{"ab12cd3e 2", true, LinearDecisionComment{Ver: "ab12cd3e", Choice: "2"}},
		{"ver=ab12cd3e 2", true, LinearDecisionComment{Ver: "ab12cd3e", Choice: "2"}},
		{"2 ver=wrong", true, LinearDecisionComment{Ver: "wrong", Choice: "2"}},
		{"a good idea", false, LinearDecisionComment{}},
		{"12", false, LinearDecisionComment{}},
		{"G", false, LinearDecisionComment{}},
		{"1.", false, LinearDecisionComment{}},
		{"✅ recorded: D-2615-1 = 1 (A: ship), ver=ab12cd3e", false, LinearDecisionComment{}},
		{"already resolved: D-2615-1 = 1 (A: ship)", false, LinearDecisionComment{}},
		{"⚠️ not recorded: this issue is closed. Reopen it to answer.", false, LinearDecisionComment{}},
		{"Superseded by ROB-2 (D-1-2, ver=ab12cd3e). Answer there: https://x", false, LinearDecisionComment{}},
		{"hello\n1", false, LinearDecisionComment{}},
		{"", false, LinearDecisionComment{}},
	}
	for _, tc := range cases {
		got, ok := ParseLinearDecisionComment(tc.body)
		if ok != tc.ok || got != tc.want {
			t.Errorf("%q: got %+v ok=%v, want %+v ok=%v", tc.body, got, ok, tc.want, tc.ok)
		}
	}
}

func decideFixture() (LinearDecisionLink, *DecisionRequest, *DecisionOptions) {
	options := twoOptions()
	request := &DecisionRequest{ID: "dr-7-1", Status: DecisionRequestOpen, Question: "ship?"}
	link := LinearDecisionLink{RequestID: "dr-7-1", TaskID: 7, Ver: DecisionRequestVer(request.Question, *options, "", nil), IssueID: "i", Identifier: "ROB-1", URL: "u"}
	return link, request, options
}

func TestDecideLinearAnswer(t *testing.T) {
	link, open, options := decideFixture()
	comment := func(mod func(*LinearDecisionAnswer)) LinearDecisionAnswer {
		in := LinearDecisionAnswer{IssueID: "i", Source: LinearDecisionSourceComment, UserID: "op", CommentID: "c1", Choice: "1"}
		if mod != nil {
			mod(&in)
		}
		return in
	}
	allMapped := map[int]bool{1: true, 2: true}
	state := func(choice string, covered map[int]bool) LinearDecisionAnswer {
		return LinearDecisionAnswer{IssueID: "i", Source: LinearDecisionSourceState, UserID: "op", EventAt: "t1", Choice: choice, StateOptions: covered}
	}
	answered := *open
	answered.Status = DecisionRequestAnswered
	answered.Resolution = &DecisionResolution{Kind: DecisionRequestAnswered, Option: "A", Linear: &DecisionLinearAudit{Source: LinearDecisionSourceComment, CommentID: "c1", IssueID: "i"}}
	byState := answered
	byState.Resolution = &DecisionResolution{Kind: DecisionRequestAnswered, Option: "A", Linear: &DecisionLinearAudit{Source: LinearDecisionSourceState, IssueID: "i", EventAt: "t1"}}
	withdrawn := *open
	withdrawn.Status = DecisionRequestWithdrawn
	withdrawn.Resolution = &DecisionResolution{Kind: DecisionRequestWithdrawn, Text: "moot"}
	superseded := link
	superseded.SupersededBy = "dr-7-2"
	newer := *open
	newer.ID = "dr-7-2"
	changed := *open
	changed.Question = "ship now?"

	cases := []struct {
		name      string
		link      LinearDecisionLink
		taskState string
		request   *DecisionRequest
		in        LinearDecisionAnswer
		outcome   string
		reply     string
		option    string
	}{
		{name: "number", link: link, request: open, in: comment(nil), outcome: LinearDecisionRecorded, reply: "✅ recorded", option: "A"},
		{name: "key", link: link, request: open, in: comment(func(a *LinearDecisionAnswer) { a.Choice = "b" }), outcome: LinearDecisionRecorded, reply: "✅ recorded", option: "B"},
		{name: "matching ref and ver", link: link, request: open, in: comment(func(a *LinearDecisionAnswer) { a.Ref, a.Ver = "d-7-1", link.Ver }), outcome: LinearDecisionRecorded, reply: "✅ recorded", option: "A"},
		{name: "wrong ver", link: link, request: open, in: comment(func(a *LinearDecisionAnswer) { a.Ver = "00000000" }), outcome: LinearDecisionRejected, reply: "⚠️ not recorded"},
		{name: "wrong ref", link: link, request: open, in: comment(func(a *LinearDecisionAnswer) { a.Ref = "D-8-1" }), outcome: LinearDecisionRejected, reply: "⚠️ not recorded"},
		{name: "ref missing its revision", link: link, request: open, in: comment(func(a *LinearDecisionAnswer) { a.Ref = "D-7" }), outcome: LinearDecisionRejected, reply: "⚠️ not recorded"},
		{name: "no such number", link: link, request: open, in: comment(func(a *LinearDecisionAnswer) { a.Choice = "3" }), outcome: LinearDecisionRejected, reply: "⚠️ not recorded"},
		{name: "number zero", link: link, request: open, in: comment(func(a *LinearDecisionAnswer) { a.Choice = "0" }), outcome: LinearDecisionRejected, reply: "⚠️ not recorded"},
		{name: "no such key", link: link, request: open, in: comment(func(a *LinearDecisionAnswer) { a.Choice = "C" }), outcome: LinearDecisionRejected, reply: "⚠️ not recorded"},
		{name: "closed issue", link: link, request: open, in: comment(func(a *LinearDecisionAnswer) { a.IssueClosed = true }), outcome: LinearDecisionRejected, reply: "⚠️ not recorded"},
		{name: "terminal task", link: link, taskState: "merged", request: open, in: comment(nil), outcome: LinearDecisionRejected, reply: "⚠️ not recorded"},
		{name: "superseded link", link: superseded, request: open, in: comment(nil), outcome: LinearDecisionSuperseded, reply: "⚠️ not recorded"},
		{name: "request replaced, link not yet marked", link: link, request: &newer, in: comment(nil), outcome: LinearDecisionSuperseded, reply: "⚠️ not recorded"},
		{name: "question differs from what the issue asked", link: link, request: &changed, in: comment(nil), outcome: LinearDecisionSuperseded, reply: "⚠️ not recorded"},
		{name: "no request", link: link, in: comment(nil), outcome: LinearDecisionSuperseded, reply: "⚠️ not recorded"},
		{name: "same comment delivered again", link: link, request: &answered, in: comment(nil), outcome: LinearDecisionDuplicate},
		{name: "later comment", link: link, request: &answered, in: comment(func(a *LinearDecisionAnswer) { a.CommentID = "c2"; a.Choice = "2" }), outcome: LinearDecisionAlreadyResolved, reply: "already resolved"},
		{name: "later comment, closed issue", link: link, request: &answered, in: comment(func(a *LinearDecisionAnswer) { a.CommentID = "c2"; a.IssueClosed = true }), outcome: LinearDecisionAlreadyResolved, reply: "already resolved"},
		{name: "comment after withdrawal", link: link, request: &withdrawn, in: comment(nil), outcome: LinearDecisionAlreadyResolved, reply: "already resolved"},
		{name: "state", link: link, request: open, in: state("2", allMapped), outcome: LinearDecisionRecorded, reply: "✅ recorded", option: "B"},
		{name: "state on a closed-type state counts", link: link, request: open, in: func() LinearDecisionAnswer { a := state("1", allMapped); a.IssueClosed = true; return a }(), outcome: LinearDecisionRecorded, reply: "✅ recorded", option: "A"},
		{name: "state with an option unmapped", link: link, request: open, in: state("1", map[int]bool{1: true}), outcome: LinearDecisionRejected, reply: "⚠️ not recorded"},
		{name: "state beyond the options", link: link, request: open, in: state("3", map[int]bool{1: true, 2: true, 3: true}), outcome: LinearDecisionRejected, reply: "⚠️ not recorded"},
		{name: "same state event again", link: link, request: &byState, in: state("1", allMapped), outcome: LinearDecisionDuplicate},
		{name: "later state event", link: link, request: &byState, in: func() LinearDecisionAnswer { a := state("2", allMapped); a.EventAt = "t2"; return a }(), outcome: LinearDecisionAlreadyResolved, reply: "already resolved"},
		{name: "state on superseded stays silent", link: superseded, request: open, in: state("1", allMapped), outcome: LinearDecisionSuperseded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := decideLinearAnswer(tc.link, tc.taskState, tc.request, options, tc.in)
			if v.outcome != tc.outcome {
				t.Fatalf("outcome=%q want %q (reply %q)", v.outcome, tc.outcome, v.reply)
			}
			if (tc.reply == "") != (v.reply == "") || !strings.HasPrefix(v.reply, tc.reply) {
				t.Fatalf("reply=%q want prefix %q", v.reply, tc.reply)
			}
			if v.option.Key != tc.option {
				t.Fatalf("option=%q want %q", v.option.Key, tc.option)
			}
			if _, parsed := ParseLinearDecisionComment(v.reply); parsed && v.reply != "" {
				t.Fatalf("reply %q would parse as an answer and could loop", v.reply)
			}
		})
	}
}

func TestDecisionRequestRefsValidation(t *testing.T) {
	base := DecisionRequestInput{Question: "q", Options: *twoOptions(), DefaultAction: "hold"}
	if err := ValidateDecisionRequestInput(base); err != nil {
		t.Fatal(err)
	}
	for name, refs := range map[string][]string{
		"too many":  strings.Split(strings.Repeat("r,", DecisionRequestMaxRefs)+"r", ","),
		"two lines": {"a\nb"},
		"blank":     {""},
		"too long":  {strings.Repeat("x", DecisionRequestActionMaxBytes+1)},
	} {
		in := base
		in.Refs = refs
		if err := ValidateDecisionRequestInput(in); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
