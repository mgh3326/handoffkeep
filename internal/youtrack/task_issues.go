package youtrack

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// The console's task pages read task issues straight from YouTrack (hk 4430).
// One page of the list carries every field those pages show and nothing else:
// no description and no comments, so a page of 500 issues stays far below
// maxBodyBytes.
const taskIssueFields = "id,idReadable,summary,created,updated,resolved,project(id,shortName)," +
	"customFields(name,$type,value(name,text,presentation,login))"

// TaskIssuePageSize is the $top of one ListTaskIssues page.
const TaskIssuePageSize = 500

// ListTaskIssues returns one page of the issues a YouTrack query matches,
// with the fields TaskFromIssue reads. A GET is idempotent, so a 5xx is
// retried like every other read.
func (c *Client) ListTaskIssues(ctx context.Context, query string, skip, top int) ([]Issue, error) {
	q := url.Values{}
	q.Set("query", query)
	q.Set("fields", taskIssueFields)
	q.Set("$skip", strconv.Itoa(skip))
	q.Set("$top", strconv.Itoa(top))
	var out []Issue
	err := c.get(ctx, "/api/issues", q, &out)
	return out, err
}

// TaskIssuesQuery is the YouTrack query for every task issue in the named
// projects, oldest first. Creation order keeps $skip paging stable while new
// issues arrive: a new issue sorts after every page already read. A name with
// braces is skipped, as in the search (YouTrack's {name} form cannot hold one).
func TaskIssuesQuery(shortNames []string) string {
	parts := make([]string, 0, len(shortNames))
	seen := map[string]bool{}
	for _, name := range shortNames {
		name = strings.TrimSpace(name)
		if name == "" || strings.ContainsAny(name, "{}") || seen[name] {
			continue
		}
		seen[name] = true
		parts = append(parts, "project: {"+name+"}")
	}
	if len(parts) == 0 {
		return ""
	}
	return "(" + strings.Join(parts, " or ") + ") sort by: created asc"
}

// TaskIssue is one task issue as the console shows it: the fields YouTrack
// owns, read from the issue alone. TaskID is the issue's hk ID field; the
// caller decides which task the issue belongs to (store.YTIssueTaskIDs).
type TaskIssue struct {
	TaskID        int64
	IssueID       string
	IssueReadable string
	ProjectShort  string
	Title         string
	// State is the hk state for the 상태 value; StateKnown is false when the
	// value maps to no hk state, and State then holds the raw value.
	State      string
	StateKnown bool
	Lane       string
	Kind       string
	Grade      string
	Tier       string
	Priority   int
	// Project is the hk project, "" for the Unassigned value.
	Project  string
	Area     string
	PR       string
	Job      string
	Stalled  bool
	Created  time.Time
	Updated  time.Time
	Resolved *time.Time
}

// TaskFromIssue reads a task issue with the field names of schema. The
// summary loses the "#<id> " prefix the write protocol adds
// (ytwrite.TaskSummary) when it names the issue's own hk ID.
func TaskFromIssue(schema Schema, issue Issue) TaskIssue {
	out := TaskIssue{IssueID: issue.ID, IssueReadable: issue.IDReadable, Title: issue.Summary}
	if issue.Project != nil {
		out.ProjectShort = issue.Project.ShortName
	}
	field := func(name string) IssueField {
		f, _ := IssueFieldByName(issue.CustomFields, name)
		return f
	}
	if n, ok := field(schema.HKIDField).IntValue(); ok && n > 0 {
		out.TaskID = int64(n)
		out.Title = strings.TrimPrefix(out.Title, "#"+strconv.Itoa(n)+" ")
	}
	raw := field(schema.StateField).StringValue()
	if state, ok := StateFromValue(raw); ok {
		out.State, out.StateKnown = state, true
	} else {
		out.State = raw
	}
	out.Lane = field(schema.LaneField).StringValue()
	out.Kind = field(schema.KindField).StringValue()
	out.Grade = field(schema.GradeField).StringValue()
	out.Tier = field(schema.TierField).StringValue()
	if n, ok := field(schema.PriorityField).IntValue(); ok {
		out.Priority = n
	}
	if project := field(schema.ProjectField).StringValue(); project != ytwrite.UnassignedProject {
		out.Project = project
	}
	out.Area = field(schema.AreaField).StringValue()
	out.PR = field(schema.PRField).StringValue()
	out.Job = field(schema.JobField).StringValue()
	out.Stalled = fieldHasValue(field(schema.StallField), StallLabel)
	out.Created = msTime(issue.Created)
	out.Updated = msTime(issue.Updated)
	if issue.Resolved != nil && *issue.Resolved > 0 {
		at := msTime(*issue.Resolved)
		out.Resolved = &at
	}
	return out
}

// fieldHasValue reports whether a single or multi enum field holds value. A
// multi-value field arrives as an array of objects.
func fieldHasValue(f IssueField, value string) bool {
	var many []json.RawMessage
	if json.Unmarshal(f.Value, &many) == nil {
		for _, one := range many {
			if (IssueField{Value: one}).StringValue() == value {
				return true
			}
		}
		return false
	}
	return f.StringValue() == value
}

func msTime(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}
