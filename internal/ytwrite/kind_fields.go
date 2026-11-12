package ytwrite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The issue_fields kind (hk 4003, docs/youtrack-write-protocol.md "Kind
// issue_fields"): the full snapshot of one task's issue (summary, description
// and every field the import set) written to the task's bound issue in one
// update. The store's change-feed consumer task_fields prepares one op per
// changed snapshot (prepareYTSnapshotTx). The op's lane is the task ("task",
// decimal id), the same lane as its comments and its issue_create, and its
// target (YTTargetID) is the bound issue's id.

func init() {
	registerKind(KindIssueFields, func(Config) Kind { return issueFields{} })
}

// KindIssueFields writes a task's issue snapshot (kind_fields.go, hk 4003).
const KindIssueFields = "issue_fields"

// ReasonFieldRefused: the project's field cannot take a snapshot value (a
// value missing from its bundle, a field the project lacks, a type the
// snapshot cannot be written as). Nothing was sent; the op is rejected.
const ReasonFieldRefused = "field_refused"

// The YouTrack field names a task issue carries: the importer's
// (youtrack/import/client.js FIELD), which the legacy push shares.
const (
	FieldHKID      = "hk ID"
	FieldProject   = "hk 프로젝트"
	FieldState     = "hk 상태"
	FieldPriority  = "hk 우선순위"
	FieldLane      = "레인"
	FieldKind      = "종류"
	FieldGrade     = "등급"
	FieldTier      = "티어"
	FieldPR        = "PR"
	FieldJob       = "작업 job"
	FieldStalled   = "멈춤"
	FieldDue       = "마감"
	FieldArea      = "영역"
	FieldMilestone = "마일스톤"
	FieldExpected  = "예상 소요"
)

// StalledValue is the 멈춤 value of a stalled task.
const StalledValue = "멈춤"

// UnassignedProject is the hk project value of a task without a project, as
// the import wrote it.
const UnassignedProject = "Unassigned"

// TaskStateLabels is the one hk state -> YouTrack 상태 value table. The keys
// are the twelve canonical store states.
var TaskStateLabels = map[string]string{
	"triage":      "분류 대기",
	"backlog":     "대기",
	"unstarted":   "착수 전",
	"researching": "조사 중",
	"developing":  "개발 중",
	"reviewing":   "검토 중",
	"merged":      "머지됨",
	"deployed":    "배포됨",
	"completed":   "완료됨",
	"dropped":     "종료됨",
	"duplicate":   "중복",
	"icebox":      "보류(보관)",
}

var titleGradeRE = regexp.MustCompile(`\(grade (SS|S\+|S|A\+|A|B|C), (T[0-3])\)\s*$`)

// TitleGradeTier reads the "(grade G, T)" marker at the end of a title.
func TitleGradeTier(title string) (grade, tier string) {
	m := titleGradeRE.FindStringSubmatch(title)
	if m == nil {
		return "", ""
	}
	return m[1], m[2]
}

// FieldType is how a snapshot value is meant; the adapter picks the wire
// shape from the project's field (a text or enum hk 프로젝트, a single or
// multi 멈춤).
type FieldType string

const (
	FieldTypeInt  FieldType = "int"
	FieldTypeText FieldType = "text"
	FieldTypeEnum FieldType = "enum"
	// FieldTypeState is a state bundle value.
	FieldTypeState FieldType = "state"
	// FieldTypeDate is epoch milliseconds, decimal.
	FieldTypeDate FieldType = "date"
	// FieldTypePeriod is whole minutes, decimal.
	FieldTypePeriod FieldType = "period"
)

// FieldValue is one field of a snapshot. Value is canonical text (decimal for
// int, date and period; the name for enum and state); "" clears the field.
type FieldValue struct {
	Name  string    `json:"name"`
	Type  FieldType `json:"type"`
	Value string    `json:"value,omitempty"`
}

// IssueSnapshot is what a task's issue says: the summary, the description
// and the fields, in a fixed order.
type IssueSnapshot struct {
	Summary     string       `json:"summary"`
	Description string       `json:"description"`
	Fields      []FieldValue `json:"fields"`
}

// IssueFieldsPayload is the immutable payload of an issue_fields op: the
// snapshot and the YouTrack project of the bound issue (the adapter reads
// its field shapes).
type IssueFieldsPayload struct {
	ProjectID string `json:"project_id"`
	IssueSnapshot
}

// TaskFields is what a snapshot is computed from: one task row and the names
// it refers to, as the store read them.
type TaskFields struct {
	TaskID int64
	// Project is the hk project ("" for none, written as Unassigned).
	Project  string
	Title    string
	TitleKO  string
	BodyDoc  string
	PR       string
	JobID    string
	ParentID int64
	State    string
	Priority int
	Lane     string
	Kind     string
	// LinearGrade and LinearTier are the refs.linear fallback when the
	// title carries no grade marker.
	LinearGrade string
	LinearTier  string
	Stalled     bool
	DueAt       *time.Time
	Area        string
	Milestone   string
	// Expected is the task's expected duration (0: none).
	Expected time.Duration
}

// FieldsText is the hk block of a task issue's description: RenderFields
// writes it, ParseFields reads it back. Every value is one line.
type FieldsText struct {
	TaskID   int64
	Project  string
	Title    string
	TitleKO  string
	BodyDoc  string
	PR       string
	JobID    string
	ParentID int64
}

const (
	fieldsKOPrefix     = "제목(한국어): "
	fieldsBodyPrefix   = "본문 문서: "
	fieldsPRPrefix     = "PR: "
	fieldsJobPrefix    = "Job: "
	fieldsParentPrefix = "Parent task: #"
)

// oneLine collapses whitespace runs (newlines included) to one space and
// trims the ends, so a value stays on its line and YouTrack keeps it as sent.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// RenderFields is the description of a task issue, a stable hk block with no
// op marker and no provenance line, so the same task renders the same text:
//
//	hk task: #<task> · project: <hk project>
//	제목(원문): <title>
//	제목(한국어): <ko>
//	본문 문서: <body document key>
//	PR: <url>
//	Job: <job id>
//	Parent task: #<parent>
//
// The lines after the title are left out when empty. Values are written one
// line each (whitespace collapsed).
func RenderFields(x FieldsText) string {
	lines := []string{
		issueTaskPrefix + strconv.FormatInt(x.TaskID, 10) + " · project: " + oneLine(x.Project),
		issueTitlePrefix + oneLine(x.Title),
	}
	add := func(prefix, v string) {
		if v = oneLine(v); v != "" {
			lines = append(lines, prefix+v)
		}
	}
	add(fieldsKOPrefix, x.TitleKO)
	add(fieldsBodyPrefix, x.BodyDoc)
	add(fieldsPRPrefix, x.PR)
	add(fieldsJobPrefix, x.JobID)
	if x.ParentID > 0 {
		lines = append(lines, fieldsParentPrefix+strconv.FormatInt(x.ParentID, 10))
	}
	return strings.Join(lines, "\n")
}

// ParseFields reads a description in exactly the form RenderFields writes; ok
// is false for anything else.
func ParseFields(description string) (x FieldsText, ok bool) {
	lines := strings.Split(description, "\n")
	if len(lines) < 2 {
		return FieldsText{}, false
	}
	idText, project, found := strings.Cut(strings.TrimPrefix(lines[0], issueTaskPrefix), " · project: ")
	if !found || !strings.HasPrefix(lines[0], issueTaskPrefix) {
		return FieldsText{}, false
	}
	id, okID := positiveDecimal(idText)
	if !okID || !strings.HasPrefix(lines[1], issueTitlePrefix) {
		return FieldsText{}, false
	}
	x = FieldsText{TaskID: id, Project: project, Title: strings.TrimPrefix(lines[1], issueTitlePrefix)}
	parent := ""
	// The optional lines, each at most once, non-empty, and in this order.
	optional := []struct {
		prefix string
		dst    *string
	}{{fieldsKOPrefix, &x.TitleKO}, {fieldsBodyPrefix, &x.BodyDoc}, {fieldsPRPrefix, &x.PR}, {fieldsJobPrefix, &x.JobID}, {fieldsParentPrefix, &parent}}
	next := 0
	for _, line := range lines[2:] {
		for next < len(optional) && !strings.HasPrefix(line, optional[next].prefix) {
			next++
		}
		if next == len(optional) || line == optional[next].prefix {
			return FieldsText{}, false
		}
		*optional[next].dst = strings.TrimPrefix(line, optional[next].prefix)
		next++
	}
	if parent != "" {
		pid, okPID := positiveDecimal(parent)
		if !okPID {
			return FieldsText{}, false
		}
		x.ParentID = pid
	}
	return x, true
}

// positiveDecimal reads a canonical positive decimal (no sign, no leading
// zero).
func positiveDecimal(s string) (int64, bool) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 || strconv.FormatInt(n, 10) != s {
		return 0, false
	}
	return n, true
}

// TaskSummary is a task issue's summary: "#<task> <title>", whitespace
// collapsed, cut to IssueSummaryLimit runes.
func TaskSummary(taskID int64, title string) string {
	return strings.TrimRight(truncateRunes("#"+strconv.FormatInt(taskID, 10)+" "+oneLine(title), IssueSummaryLimit), " ")
}

// TaskIssueSnapshot is the snapshot of t's issue with the import's value
// mapping (youtrack/import/client.js issue()): the state as its Korean
// label, the grade and tier from the title marker or refs.linear, 멈춤 when
// stalled. A field with no value is in the list with "" so the update clears
// it.
func TaskIssueSnapshot(t TaskFields) IssueSnapshot {
	project := t.Project
	if project == "" {
		project = UnassignedProject
	}
	state := t.State
	if label, ok := TaskStateLabels[state]; ok {
		state = label
	}
	grade, tier := TitleGradeTier(t.Title)
	if grade == "" {
		grade, tier = t.LinearGrade, t.LinearTier
	}
	stalled := ""
	if t.Stalled {
		stalled = StalledValue
	}
	due := ""
	if t.DueAt != nil {
		local := t.DueAt.In(time.FixedZone("Asia/Seoul", 9*60*60))
		due = strconv.FormatInt(time.Date(local.Year(), local.Month(), local.Day(), 12, 0, 0, 0, time.UTC).UnixMilli(), 10)
	}
	expected := ""
	if m := int64(t.Expected / time.Minute); m > 0 {
		expected = strconv.FormatInt(m, 10)
	}
	return IssueSnapshot{
		Summary: TaskSummary(t.TaskID, t.Title),
		Description: RenderFields(FieldsText{TaskID: t.TaskID, Project: project, Title: t.Title, TitleKO: t.TitleKO,
			BodyDoc: t.BodyDoc, PR: t.PR, JobID: t.JobID, ParentID: t.ParentID}),
		Fields: []FieldValue{
			{FieldHKID, FieldTypeInt, strconv.FormatInt(t.TaskID, 10)},
			{FieldProject, FieldTypeText, project},
			{FieldState, FieldTypeState, state},
			{FieldPriority, FieldTypeInt, strconv.Itoa(t.Priority)},
			{FieldLane, FieldTypeEnum, t.Lane},
			{FieldKind, FieldTypeEnum, t.Kind},
			{FieldGrade, FieldTypeEnum, grade},
			{FieldTier, FieldTypeEnum, tier},
			{FieldPR, FieldTypeText, oneLine(t.PR)},
			{FieldJob, FieldTypeText, oneLine(t.JobID)},
			{FieldStalled, FieldTypeEnum, stalled},
			{FieldDue, FieldTypeDate, due},
			{FieldArea, FieldTypeText, t.Area},
			{FieldMilestone, FieldTypeText, oneLine(t.Milestone)},
			{FieldExpected, FieldTypePeriod, expected},
		},
	}
}

// FieldsRequest is one task snapshot written to its bound issue.
type FieldsRequest struct {
	Principal string
	Actor     string
	// Tenant, IssueID and ProjectID are the task's binding.
	Tenant    string
	IssueID   string
	ProjectID string
	Task      TaskFields
}

// CheckFields explains why a fields request cannot be stored.
func CheckFields(req FieldsRequest) error {
	if !principalRE.MatchString(req.Principal) {
		return fmt.Errorf("%w: principal is empty or malformed", ErrInvalid)
	}
	if err := checkActor(req.Actor); err != nil {
		return err
	}
	if req.Task.TaskID <= 0 {
		return fmt.Errorf("%w: task id must be positive", ErrInvalid)
	}
	if !issueIDRE.MatchString(req.IssueID) || !issueIDRE.MatchString(req.ProjectID) {
		return fmt.Errorf("%w: youtrack issue or project id is empty or malformed", ErrInvalid)
	}
	if req.Tenant == "" {
		return fmt.Errorf("%w: tenant is empty", ErrInvalid)
	}
	return nil
}

// FieldsOp is the op a fields request stores, with no Key: the store's
// snapshot helper sets it (SnapshotKey) from PayloadSHA, which covers the
// whole snapshot, the target issue and the tenant.
func FieldsOp(req FieldsRequest) (NewOp, error) {
	if err := CheckFields(req); err != nil {
		return NewOp{}, err
	}
	payload, err := json.Marshal(IssueFieldsPayload{ProjectID: req.ProjectID, IssueSnapshot: TaskIssueSnapshot(req.Task)})
	if err != nil {
		return NewOp{}, err
	}
	entityID := strconv.FormatInt(req.Task.TaskID, 10)
	return NewOp{
		Principal:  req.Principal,
		Kind:       KindIssueFields,
		PayloadSHA: payloadSHA(KindIssueFields, "task", entityID, req.IssueID, req.Tenant, req.Actor, string(payload)),
		Payload:    payload,
		EntityKind: "task",
		EntityID:   entityID,
		YTTargetID: req.IssueID,
		Tenant:     req.Tenant,
		Actor:      req.Actor,
	}, nil
}

// IssueFieldsUpdate is one issue update: the snapshot written to IssueID, a
// ProjectID issue.
type IssueFieldsUpdate struct {
	IssueID   string
	ProjectID string
	IssueSnapshot
}

// IssueFieldsRemote is the YouTrack surface of the issue_fields kind. A
// Remote that does not implement it cannot send that kind.
type IssueFieldsRemote interface {
	// UpdateIssueFields sets the summary, the description and every field
	// of the snapshot (a "" value clears the field) in exactly one POST and
	// returns the issue's id. It may read the project's fields first: a
	// failed read is a *NotSentError, and a value the project's field cannot
	// take is a *RefusedError (ReasonFieldRefused) naming the field and the
	// value; neither sends anything. Other errors follow CreateComment's.
	UpdateIssueFields(ctx context.Context, in IssueFieldsUpdate) (id string, err error)
	// ReadIssueFields reads the issue's summary, description and fields,
	// each field's value in the canonical text of FieldValue ("" for none).
	// A 429 is a *RemoteError with the server's RetryAfter.
	ReadIssueFields(ctx context.Context, issueID string) (IssueSnapshot, error)
}

// issueFields is the issue_fields kind. Its text (Render) is the
// description; Send and Evidence take the whole snapshot from the payload.
type issueFields struct{}

func fieldsPayloadOf(op Op) (IssueFieldsPayload, error) {
	var p IssueFieldsPayload
	if err := json.Unmarshal(op.Payload, &p); err != nil {
		return p, fmt.Errorf("%w: payload of op %d: %v", ErrInvalid, op.ID, err)
	}
	if op.EntityKind != "task" || !issueIDRE.MatchString(op.YTTargetID) || p.Summary == "" {
		return p, fmt.Errorf("%w: op %d is not a task issue snapshot", ErrInvalid, op.ID)
	}
	return p, nil
}

func (issueFields) Render(op Op) (string, error) {
	p, err := fieldsPayloadOf(op)
	if err != nil {
		return "", err
	}
	return p.Description, nil
}

func (issueFields) Send(ctx context.Context, remote Remote, op Op, _ string) (string, error) {
	fr, ok := remote.(IssueFieldsRemote)
	if !ok {
		return "", &RefusedError{Reason: ReasonUnsupportedKind, Detail: "the remote cannot update issue fields"}
	}
	p, err := fieldsPayloadOf(op)
	if err != nil {
		return "", &RefusedError{Reason: ReasonInvalidIssue, Detail: err.Error()}
	}
	return fr.UpdateIssueFields(ctx, IssueFieldsUpdate{IssueID: op.YTTargetID, ProjectID: p.ProjectID, IssueSnapshot: p.IssueSnapshot})
}

// Evidence reads the issue: when its summary, its description and every
// snapshot field equal the op's, the issue is the one match; anything else
// is one mismatch, which the shared rule keeps remote_unknown (an identical
// resend would be harmless, but the protocol never resends on its own).
func (issueFields) Evidence(ctx context.Context, remote Remote, op Op, _ string) (Evidence, error) {
	var ev Evidence
	fr, ok := remote.(IssueFieldsRemote)
	if !ok {
		return ev, errors.New("the remote cannot read issue fields")
	}
	p, err := fieldsPayloadOf(op)
	if err != nil {
		return ev, err
	}
	got, err := fr.ReadIssueFields(ctx, op.YTTargetID)
	if err != nil {
		var re *RemoteError
		if errors.As(err, &re) && re.Status == http.StatusTooManyRequests {
			ev.RetryAfter = re.RetryAfter
		}
		return ev, err
	}
	if SnapshotDiff(p.IssueSnapshot, got) == "" {
		ev.Matches = []string{op.YTTargetID}
	} else {
		ev.Mismatched = 1
	}
	return ev, nil
}

// SnapshotDiff names the first part of want that got does not equal (""
// when every part does). A field got lacks counts as "".
func SnapshotDiff(want, got IssueSnapshot) string {
	if want.Summary != got.Summary {
		return "summary"
	}
	if want.Description != got.Description {
		return "description"
	}
	have := make(map[string]string, len(got.Fields))
	for _, f := range got.Fields {
		have[f.Name] = f.Value
	}
	for _, f := range want.Fields {
		if !snapshotFieldEqual(f.Name, f.Value, have[f.Name]) {
			return f.Name
		}
	}
	return ""
}

// YouTrack's date field stores a date as noon UTC. Compare 마감 by the UTC
// calendar date it represents; every other snapshot field remains exact.
func snapshotFieldEqual(name, want, got string) bool {
	if name != FieldDue || want == "" || got == "" {
		return want == got
	}
	wantMS, wantErr := strconv.ParseInt(want, 10, 64)
	gotMS, gotErr := strconv.ParseInt(got, 10, 64)
	if wantErr != nil || gotErr != nil {
		return false
	}
	wy, wm, wd := time.UnixMilli(wantMS).UTC().Date()
	gy, gm, gd := time.UnixMilli(gotMS).UTC().Date()
	return wy == gy && wm == gm && wd == gd
}

// MapRows: an update creates nothing to map.
func (issueFields) MapRows(Op, string) []MapRow { return nil }
