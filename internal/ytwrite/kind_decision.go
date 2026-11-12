package ytwrite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// The decision kinds (hk 4054, docs/youtrack-write-protocol.md "Kinds
// decision_create and decision_fields"): a decision request's DEC issue
// carries its whole text (the full question, the numbered options, the
// recommendation, the default action and the answer), which the import cut to
// a 250-rune summary. decision_create makes the issue at most once per
// decision (key DecisionKey) or adopts the one DEC issue that already carries
// the decision's 결정 ID; decision_fields writes the full snapshot to the
// bound issue, as issue_fields does for a task. Both ride the decision's lane
// ("decision", decimal decision_requests id). Arbitration, receipts, option
// ids and question hashes stay in hk.

func init() {
	registerKind(KindDecisionCreate, func(Config) Kind { return decisionCreate{} })
	registerKind(KindDecisionFields, func(Config) Kind { return issueFields{entityKind: DecisionEntity} })
}

const (
	// KindDecisionCreate creates (or adopts) a decision's DEC issue.
	KindDecisionCreate = "decision_create"
	// KindDecisionFields writes a decision's DEC issue snapshot.
	KindDecisionFields = "decision_fields"
	// DecisionEntity is the lane entity kind of the decision ops.
	DecisionEntity = "decision"
)

// The DEC project's field names (youtrack/import/client.js FIELD), with
// FieldHKID and FieldLane shared with the task projects.
const (
	FieldDecisionID    = "결정 ID"
	FieldDecisionState = "결정 상태"
	FieldRecommended   = "권장"
	FieldDecisionDue   = "결정 기한"
	FieldLinkedTask    = "연결 task ID"
)

// DecisionStateLabels is the 결정 상태 value of each decision status (the
// import's DECISION_STATE). expired is the read-time status of an open
// request past its expires_at.
var DecisionStateLabels = map[string]string{
	"open":       "열림",
	"answered":   "답변됨",
	"superseded": "대체됨",
	"expired":    "만료됨",
}

// DecisionKey is the idempotency key of a decision's decision_create op: one
// create per decision.
func DecisionKey(decisionID int64) string {
	return "decision-issue:" + strconv.FormatInt(decisionID, 10)
}

// DecisionOption is one choice as the issue shows it. The option ids and
// their arbitration stay in hk.
type DecisionOption struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}

// DecisionText is the hk block of a DEC issue's description. RenderDecision
// writes it and ParseDecision reads it back; CanonicalDecision is the form
// both agree on (ParseDecision(RenderDecision(x)) == CanonicalDecision(x)).
// Question and Answer keep their lines; every other value is one line.
type DecisionText struct {
	DecisionID int64 `json:"decision_id"`
	// TaskID is the task that asked (0: none).
	TaskID        int64            `json:"task_id,omitempty"`
	Question      string           `json:"question"`
	Options       []DecisionOption `json:"options,omitempty"`
	Recommended   string           `json:"recommended,omitempty"`
	DefaultAction string           `json:"default_action,omitempty"`
	Answer        string           `json:"answer,omitempty"`
	AnsweredBy    string           `json:"answered_by,omitempty"`
	// Channel is the answer's channel (decision_resolutions.channel), when
	// one is recorded.
	Channel string `json:"channel,omitempty"`
	// AnsweredAt is UTC, at second precision.
	AnsweredAt *time.Time `json:"answered_at,omitempty"`
}

const (
	decisionHeadPrefix     = "hk decision: D-"
	decisionTaskPrefix     = "연결 작업: #"
	decisionQuestionLine   = "질문:"
	decisionOptionsLine    = "선택지:"
	decisionRecommendedPfx = "권장: "
	decisionDefaultPfx     = "기본 동작: "
	decisionAnswerLine     = "답:"
	decisionByPrefix       = "답한 사람: "
	decisionChannelPrefix  = "답한 경로: "
	decisionAtPrefix       = "답한 시각: "
	decisionQuote          = ">"
)

// multiLine is s with its lines kept: CR LF and CR become LF, each line loses
// its trailing whitespace, and leading and trailing empty lines are dropped,
// so YouTrack keeps the text as sent.
func multiLine(s string) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRightFunc(l, unicode.IsSpace)
	}
	for len(lines) > 0 && lines[0] == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
}

// CanonicalDecision is x in the form RenderDecision writes: the question and
// the answer through multiLine, every other text on one line, an option key
// without "]" (the key's closing bracket), the time in UTC at second
// precision, and no options as nil.
func CanonicalDecision(x DecisionText) DecisionText {
	out := DecisionText{
		DecisionID:    x.DecisionID,
		TaskID:        x.TaskID,
		Question:      multiLine(x.Question),
		Recommended:   oneLine(x.Recommended),
		DefaultAction: oneLine(x.DefaultAction),
		Answer:        multiLine(x.Answer),
		AnsweredBy:    oneLine(x.AnsweredBy),
		Channel:       oneLine(x.Channel),
	}
	if out.TaskID < 0 {
		out.TaskID = 0
	}
	for _, o := range x.Options {
		out.Options = append(out.Options, DecisionOption{Key: oneLine(strings.ReplaceAll(o.Key, "]", "")), Label: oneLine(o.Label)})
	}
	if x.AnsweredAt != nil {
		at := x.AnsweredAt.UTC().Truncate(time.Second)
		out.AnsweredAt = &at
	}
	return out
}

// quoteLines writes each line of a multiLine text as "> line" (">" for an
// empty one), so no line of the text can read as a line of the block.
func quoteLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if l == "" {
			lines[i] = decisionQuote
		} else {
			lines[i] = decisionQuote + " " + l
		}
	}
	return lines
}

// RenderDecision is the hk block of a DEC issue's description, of
// CanonicalDecision(x):
//
//	hk decision: D-<id>
//	연결 작업: #<task>
//	질문:
//	> <question, line by line>
//	선택지:
//	1. [<key>] <label>
//	권장: <recommended>
//	기본 동작: <default action>
//	답:
//	> <answer, line by line>
//	답한 사람: <who>
//	답한 경로: <channel>
//	답한 시각: <UTC>
//
// The 질문: line is always there; every other line (or section) is left out
// when its value is empty.
func RenderDecision(x DecisionText) string {
	x = CanonicalDecision(x)
	lines := []string{decisionHeadPrefix + strconv.FormatInt(x.DecisionID, 10)}
	if x.TaskID > 0 {
		lines = append(lines, decisionTaskPrefix+strconv.FormatInt(x.TaskID, 10))
	}
	lines = append(lines, decisionQuestionLine)
	lines = append(lines, quoteLines(x.Question)...)
	if len(x.Options) > 0 {
		lines = append(lines, decisionOptionsLine)
		for i, o := range x.Options {
			line := strconv.Itoa(i+1) + ". [" + o.Key + "]"
			if o.Label != "" {
				line += " " + o.Label
			}
			lines = append(lines, line)
		}
	}
	add := func(prefix, v string) {
		if v != "" {
			lines = append(lines, prefix+v)
		}
	}
	add(decisionRecommendedPfx, x.Recommended)
	add(decisionDefaultPfx, x.DefaultAction)
	if x.Answer != "" {
		lines = append(lines, decisionAnswerLine)
		lines = append(lines, quoteLines(x.Answer)...)
	}
	add(decisionByPrefix, x.AnsweredBy)
	add(decisionChannelPrefix, x.Channel)
	if x.AnsweredAt != nil {
		lines = append(lines, decisionAtPrefix+x.AnsweredAt.Format(ProvenanceTimeLayout))
	}
	return strings.Join(lines, "\n")
}

// ParseDecision reads a block in exactly the form RenderDecision writes; ok
// is false for anything else.
func ParseDecision(block string) (x DecisionText, ok bool) {
	lines := strings.Split(block, "\n")
	i := 0
	next := func() (string, bool) {
		if i >= len(lines) {
			return "", false
		}
		i++
		return lines[i-1], true
	}
	peek := func() string {
		if i >= len(lines) {
			return ""
		}
		return lines[i]
	}
	quoted := func() string {
		var out []string
		for i < len(lines) && (lines[i] == decisionQuote || strings.HasPrefix(lines[i], decisionQuote+" ")) {
			if lines[i] == decisionQuote {
				out = append(out, "")
			} else {
				out = append(out, strings.TrimPrefix(lines[i], decisionQuote+" "))
			}
			i++
		}
		return strings.Join(out, "\n")
	}
	head, _ := next()
	id, okID := positiveDecimal(strings.TrimPrefix(head, decisionHeadPrefix))
	if !okID || !strings.HasPrefix(head, decisionHeadPrefix) {
		return DecisionText{}, false
	}
	x.DecisionID = id
	if l := peek(); strings.HasPrefix(l, decisionTaskPrefix) {
		task, okTask := positiveDecimal(strings.TrimPrefix(l, decisionTaskPrefix))
		if !okTask {
			return DecisionText{}, false
		}
		x.TaskID = task
		i++
	}
	if l, _ := next(); l != decisionQuestionLine {
		return DecisionText{}, false
	}
	x.Question = quoted()
	if peek() == decisionOptionsLine {
		i++
		for n := 1; ; n++ {
			l := peek()
			rest, found := strings.CutPrefix(l, strconv.Itoa(n)+". [")
			if !found {
				break
			}
			key, label, closed := strings.Cut(rest, "]")
			if !closed || (label != "" && !strings.HasPrefix(label, " ")) || label == " " {
				return DecisionText{}, false
			}
			x.Options = append(x.Options, DecisionOption{Key: key, Label: strings.TrimPrefix(label, " ")})
			i++
		}
		if len(x.Options) == 0 {
			return DecisionText{}, false
		}
	}
	optional := func(prefix string, dst *string) bool {
		l := peek()
		if !strings.HasPrefix(l, prefix) {
			return true
		}
		if l == prefix {
			return false
		}
		*dst = strings.TrimPrefix(l, prefix)
		i++
		return true
	}
	if !optional(decisionRecommendedPfx, &x.Recommended) || !optional(decisionDefaultPfx, &x.DefaultAction) {
		return DecisionText{}, false
	}
	if peek() == decisionAnswerLine {
		i++
		if x.Answer = quoted(); x.Answer == "" {
			return DecisionText{}, false
		}
	}
	at := ""
	if !optional(decisionByPrefix, &x.AnsweredBy) || !optional(decisionChannelPrefix, &x.Channel) || !optional(decisionAtPrefix, &at) {
		return DecisionText{}, false
	}
	if at != "" {
		t, err := time.Parse(ProvenanceTimeLayout, at)
		if err != nil || t.UTC().Format(ProvenanceTimeLayout) != at {
			return DecisionText{}, false
		}
		t = t.UTC()
		x.AnsweredAt = &t
	}
	if i != len(lines) {
		return DecisionText{}, false
	}
	if c := CanonicalDecision(x); !decisionTextEqual(c, x) {
		return DecisionText{}, false
	}
	return x, true
}

// decisionTextEqual compares two decision texts field by field.
func decisionTextEqual(a, b DecisionText) bool {
	if a.DecisionID != b.DecisionID || a.TaskID != b.TaskID || a.Question != b.Question || a.Recommended != b.Recommended ||
		a.DefaultAction != b.DefaultAction || a.Answer != b.Answer || a.AnsweredBy != b.AnsweredBy || a.Channel != b.Channel ||
		len(a.Options) != len(b.Options) || (a.AnsweredAt == nil) != (b.AnsweredAt == nil) {
		return false
	}
	if a.AnsweredAt != nil && !a.AnsweredAt.Equal(*b.AnsweredAt) {
		return false
	}
	for i := range a.Options {
		if a.Options[i] != b.Options[i] {
			return false
		}
	}
	return true
}

// DecisionSummary is a DEC issue's summary: "D-<id> <question>", the question
// on one line, cut to IssueSummaryLimit runes (the import's form).
func DecisionSummary(decisionID int64, question string) string {
	return strings.TrimRight(truncateRunes("D-"+strconv.FormatInt(decisionID, 10)+" "+oneLine(question), IssueSummaryLimit), " ")
}

// SummaryNamesDecision reports whether a summary names the decision, in the
// import's form "D-<id>" or the legacy `youtrack sync` mirror's
// "[결정 <id>]", then a space or the end ("D-37" does not name decision 3).
func SummaryNamesDecision(summary string, decisionID int64) bool {
	id := strconv.FormatInt(decisionID, 10)
	for _, prefix := range []string{"D-" + id, "[결정 " + id + "]"} {
		rest, ok := strings.CutPrefix(summary, prefix)
		if !ok {
			continue
		}
		if rest == "" {
			return true
		}
		r, _ := utf8.DecodeRuneInString(rest)
		return unicode.IsSpace(r)
	}
	return false
}

// checkDecisionText explains why a decision's text cannot be written: no
// marker anywhere (marker-shaped text is data, never evidence), valid UTF-8
// without NUL, and a block within MaxBodyBytes.
func checkDecisionText(x DecisionText) error {
	if x.DecisionID <= 0 {
		return fmt.Errorf("%w: decision id must be positive", ErrInvalid)
	}
	block := RenderDecision(x)
	if !utf8.ValidString(block) || strings.ContainsRune(block, 0) {
		return fmt.Errorf("%w: decision text is not valid UTF-8 without NUL", ErrInvalid)
	}
	if strings.Contains(block, markerPrefix) {
		return fmt.Errorf("%w: decision text contains the reserved hk op marker", ErrInvalid)
	}
	if len(block) > MaxBodyBytes {
		return fmt.Errorf("%w: decision text is over %d bytes", ErrInvalid, MaxBodyBytes)
	}
	return nil
}

// DecisionFields is what a decision's snapshot is computed from, as the store
// read it: its text, its status (open, answered, superseded, or expired, the
// read-time status), its lane and its due time.
type DecisionFields struct {
	DecisionText
	Status string
	Lane   string
	DueAt  *time.Time
}

// DecisionIssueSnapshot is the snapshot of a decision's DEC issue with the
// import's value mapping (youtrack/import/client.js decisionIssues): the
// summary, the description (RenderDecision, no op marker, no provenance line,
// so the same decision renders the same text) and every field, "" clearing
// it.
func DecisionIssueSnapshot(d DecisionFields) IssueSnapshot {
	x := CanonicalDecision(d.DecisionText)
	state := d.Status
	if label, ok := DecisionStateLabels[state]; ok {
		state = label
	}
	due, task := "", ""
	if d.DueAt != nil {
		due = strconv.FormatInt(d.DueAt.UTC().UnixMilli(), 10)
	}
	if x.TaskID > 0 {
		task = strconv.FormatInt(x.TaskID, 10)
	}
	id := strconv.FormatInt(x.DecisionID, 10)
	return IssueSnapshot{
		Summary:     DecisionSummary(x.DecisionID, x.Question),
		Description: RenderDecision(x),
		Fields: []FieldValue{
			{FieldHKID, FieldTypeInt, id},
			{FieldDecisionID, FieldTypeInt, id},
			{FieldDecisionState, FieldTypeEnum, state},
			{FieldLane, FieldTypeEnum, oneLine(d.Lane)},
			{FieldRecommended, FieldTypeText, x.Recommended},
			{FieldDecisionDue, FieldTypeDate, due},
			{FieldLinkedTask, FieldTypeInt, task},
		},
	}
}

// DecisionRequest is one decision whose DEC issue hk creates.
type DecisionRequest struct {
	Principal string
	// Actor is who the provenance line names.
	Actor string
	// YTProjectID and YTProjectShort are the DEC project; Tenant the
	// YouTrack host.
	YTProjectID    string
	YTProjectShort string
	Tenant         string
	Decision       DecisionText
}

// DecisionCreatePayload is the immutable payload of a decision_create op: the
// decision's text as it was when the op was prepared, and the DEC project's
// short name (the search scope). The decision id is the op's EntityID and the
// project id its YTTargetID.
type DecisionCreatePayload struct {
	Decision       DecisionText `json:"decision"`
	YTProjectShort string       `json:"yt_project_short"`
}

// CheckDecision explains why a decision request cannot be sent. A store that
// must record such a decision builds the op with DecisionOp and rejects it in
// the same transaction.
func CheckDecision(req DecisionRequest) error {
	if !principalRE.MatchString(req.Principal) {
		return fmt.Errorf("%w: principal is empty or malformed", ErrInvalid)
	}
	if err := checkActor(req.Actor); err != nil {
		return err
	}
	if !issueIDRE.MatchString(req.YTProjectID) || !issueIDRE.MatchString(req.YTProjectShort) {
		return fmt.Errorf("%w: youtrack project id or short name is empty or malformed", ErrInvalid)
	}
	if req.Tenant == "" {
		return fmt.Errorf("%w: tenant is empty", ErrInvalid)
	}
	return checkDecisionText(req.Decision)
}

// DecisionOp is the op a decision request stores. Like IssueOp it does not
// validate the request beyond what it needs to build the op.
func DecisionOp(req DecisionRequest) (NewOp, error) {
	if req.Decision.DecisionID <= 0 {
		return NewOp{}, fmt.Errorf("%w: decision id must be positive", ErrInvalid)
	}
	payload, err := json.Marshal(DecisionCreatePayload{Decision: CanonicalDecision(req.Decision), YTProjectShort: req.YTProjectShort})
	if err != nil {
		return NewOp{}, err
	}
	entityID := strconv.FormatInt(req.Decision.DecisionID, 10)
	return NewOp{
		Principal:  req.Principal,
		Kind:       KindDecisionCreate,
		Key:        DecisionKey(req.Decision.DecisionID),
		PayloadSHA: payloadSHA(KindDecisionCreate, DecisionEntity, entityID, req.YTProjectID, req.Tenant, req.Actor, string(payload)),
		Payload:    payload,
		EntityKind: DecisionEntity,
		EntityID:   entityID,
		YTTargetID: req.YTProjectID,
		Tenant:     req.Tenant,
		Actor:      req.Actor,
	}, nil
}

// DecisionFieldsRequest is one decision snapshot written to its bound issue.
type DecisionFieldsRequest struct {
	Principal string
	Actor     string
	// Tenant, IssueID and ProjectID are the decision's binding.
	Tenant    string
	IssueID   string
	ProjectID string
	Decision  DecisionFields
}

// DecisionFieldsOp is the op a decision fields request stores, with no Key:
// the store's snapshot helper sets it from PayloadSHA, which covers the whole
// snapshot, the target issue and the tenant.
func DecisionFieldsOp(req DecisionFieldsRequest) (NewOp, error) {
	if !principalRE.MatchString(req.Principal) {
		return NewOp{}, fmt.Errorf("%w: principal is empty or malformed", ErrInvalid)
	}
	if err := checkActor(req.Actor); err != nil {
		return NewOp{}, err
	}
	if !issueIDRE.MatchString(req.IssueID) || !issueIDRE.MatchString(req.ProjectID) {
		return NewOp{}, fmt.Errorf("%w: youtrack issue or project id is empty or malformed", ErrInvalid)
	}
	if req.Tenant == "" {
		return NewOp{}, fmt.Errorf("%w: tenant is empty", ErrInvalid)
	}
	if err := checkDecisionText(req.Decision.DecisionText); err != nil {
		return NewOp{}, err
	}
	payload, err := json.Marshal(IssueFieldsPayload{ProjectID: req.ProjectID, IssueSnapshot: DecisionIssueSnapshot(req.Decision)})
	if err != nil {
		return NewOp{}, err
	}
	entityID := strconv.FormatInt(req.Decision.DecisionID, 10)
	return NewOp{
		Principal:  req.Principal,
		Kind:       KindDecisionFields,
		PayloadSHA: payloadSHA(KindDecisionFields, DecisionEntity, entityID, req.IssueID, req.Tenant, req.Actor, string(payload)),
		Payload:    payload,
		EntityKind: DecisionEntity,
		EntityID:   entityID,
		YTTargetID: req.IssueID,
		Tenant:     req.Tenant,
		Actor:      req.Actor,
	}, nil
}

// DecisionIssueCreate is one DEC issue create: the project, the text and the
// decision whose hk ID and 결정 ID the issue carries (the only fields a create
// writes; the rest is the decision_fields snapshot's).
type DecisionIssueCreate struct {
	ProjectID   string
	Summary     string
	Description string
	DecisionID  int64
}

// DecisionRemote is the YouTrack surface of the decision_create kind. A
// Remote that does not implement it cannot send that kind.
type DecisionRemote interface {
	// CheckDecisionFields reports whether the project has the hk ID and
	// 결정 ID fields the search and the create use. A missing field is a
	// plain error (the op is held until the schema is repaired); a failed
	// read follows CreateComment's errors.
	CheckDecisionFields(ctx context.Context, projectID string) error
	// FindDecisionIssues returns every issue of the project (short name and
	// id) whose 결정 ID is decisionID, checked against both, or an error. A
	// result it cannot prove complete is an error; a 429 is a *RemoteError
	// with the server's RetryAfter.
	FindDecisionIssues(ctx context.Context, projectShort, projectID string, decisionID int64) ([]RemoteIssue, error)
	// CreateDecisionIssue makes exactly one POST and returns the new
	// issue's id. Its errors follow CreateComment's.
	CreateDecisionIssue(ctx context.Context, in DecisionIssueCreate) (id string, err error)
}

// decisionCreate is the decision_create kind. Its text (Render) is the
// description, which carries the provenance line and the marker; Send derives
// the summary from the same op.
type decisionCreate struct{}

func decisionCreateOf(op Op) (DecisionText, DecisionCreatePayload, error) {
	var p DecisionCreatePayload
	if err := json.Unmarshal(op.Payload, &p); err != nil {
		return DecisionText{}, p, fmt.Errorf("%w: payload of op %d: %v", ErrInvalid, op.ID, err)
	}
	id, err := strconv.ParseInt(op.EntityID, 10, 64)
	if err != nil || id <= 0 || op.EntityKind != DecisionEntity || p.Decision.DecisionID != id {
		return DecisionText{}, p, fmt.Errorf("%w: op %d is not on its decision's lane", ErrInvalid, op.ID)
	}
	return p.Decision, p, nil
}

// RenderDecisionCreate is the description a decision_create op writes:
//
//	작성: <actor> · <UTC> · hk op <op>
//	<RenderDecision block>
//
//	<!-- hk-op:<op> -->
func RenderDecisionCreate(actor string, at time.Time, opID int64, x DecisionText) string {
	return ProvenanceLine(actor, at, opID) + "\n" + RenderDecision(x) + "\n\n" + OpMarker(opID)
}

func (decisionCreate) Render(op Op) (string, error) {
	x, _, err := decisionCreateOf(op)
	if err != nil {
		return "", err
	}
	if err := checkDecisionText(x); err != nil {
		return "", err
	}
	return RenderDecisionCreate(op.Actor, op.CreatedAt.UTC().Truncate(time.Second), op.ID, x), nil
}

// Send checks the project's fields, then looks for the decision's issue and
// writes only when there is none: exactly one issue with the decision's 결정
// ID whose summary names it (the import's DEC-<id>, or the legacy mirror's
// "[결정 <id>]" issue) is adopted (no POST); any
// other hit is ambiguous_issue (no POST); no hit is exactly one create with
// the summary, the description and only the hk ID and 결정 ID fields.
func (decisionCreate) Send(ctx context.Context, remote Remote, op Op, text string) (string, error) {
	dr, ok := remote.(DecisionRemote)
	if !ok {
		return "", &RefusedError{Reason: ReasonUnsupportedKind, Detail: "the remote cannot create decision issues"}
	}
	x, p, err := decisionCreateOf(op)
	if err != nil {
		return "", &RefusedError{Reason: ReasonInvalidIssue, Detail: err.Error()}
	}
	if err := dr.CheckDecisionFields(ctx, op.YTTargetID); err != nil {
		return "", &NotSentError{Err: err}
	}
	hits, err := dr.FindDecisionIssues(ctx, p.YTProjectShort, op.YTTargetID, x.DecisionID)
	if err != nil {
		return "", &NotSentError{Err: err}
	}
	switch {
	case len(hits) == 1 && SummaryNamesDecision(hits[0].Summary, x.DecisionID):
		return "", &AdoptedError{ID: hits[0].ID}
	case len(hits) > 0:
		ids := make([]string, 0, len(hits))
		for _, h := range hits {
			ids = append(ids, h.ID)
		}
		return "", &RefusedError{Reason: ReasonAmbiguousIssue, Detail: fmt.Sprintf("project %s has %d issues with 결정 ID %d and none can be adopted: %s", op.YTTargetID, len(hits), x.DecisionID, strings.Join(ids, ","))}
	}
	return dr.CreateDecisionIssue(ctx, DecisionIssueCreate{ProjectID: op.YTTargetID, Summary: DecisionSummary(x.DecisionID, x.Question), Description: text, DecisionID: x.DecisionID})
}

// Evidence lists the project's issues with the decision's 결정 ID. Exactly
// one, whose description is exactly the rendered text, is the match; every
// other issue with that 결정 ID counts as a mismatch, so the shared rule keeps
// the op remote_unknown unless that one issue is all there is.
func (decisionCreate) Evidence(ctx context.Context, remote Remote, op Op, text string) (Evidence, error) {
	var ev Evidence
	dr, ok := remote.(DecisionRemote)
	if !ok {
		return ev, errors.New("the remote cannot list decision issues")
	}
	x, p, err := decisionCreateOf(op)
	if err != nil {
		return ev, err
	}
	hits, err := dr.FindDecisionIssues(ctx, p.YTProjectShort, op.YTTargetID, x.DecisionID)
	if err != nil {
		var re *RemoteError
		if errors.As(err, &re) && re.Status == http.StatusTooManyRequests {
			ev.RetryAfter = re.RetryAfter
		}
		return ev, err
	}
	for _, h := range hits {
		if h.Description == text {
			ev.Matches = append(ev.Matches, h.ID)
		} else {
			ev.Mismatched++
		}
	}
	return ev, nil
}

// MapRows maps the decision to its issue, with the DEC project as parent.
func (decisionCreate) MapRows(op Op, ytID string) []MapRow {
	return []MapRow{{EntityKind: DecisionEntity, HKKey: op.EntityID, YTID: ytID, YTParentID: op.YTTargetID, OpID: op.ID}}
}
