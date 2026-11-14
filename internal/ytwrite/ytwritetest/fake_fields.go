package ytwritetest

import (
	"context"
	"fmt"
	"net/http"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

var _ ytwrite.IssueFieldsRemote = (*FakeYouTrack)(nil)

// SetBundle makes the enum or state field name accept only values: an
// update setting another value is refused with a 400, as YouTrack refuses a
// value missing from the field's bundle.
func (f *FakeYouTrack) SetBundle(name string, values ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	set := map[string]bool{}
	for _, v := range values {
		set[v] = true
	}
	f.bundles[name] = set
}

// Updates returns every IssueFieldsUpdate the fake received, in order.
func (f *FakeYouTrack) Updates() []ytwrite.IssueFieldsUpdate {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ytwrite.IssueFieldsUpdate(nil), f.updates...)
}

// Reads is the number of ReadIssueFields calls received.
func (f *FakeYouTrack) Reads() int { f.mu.Lock(); defer f.mu.Unlock(); return f.reads }

// QueueRead makes the next ReadIssueFields calls return errs, in order.
func (f *FakeYouTrack) QueueRead(errs ...error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readFaults = append(f.readFaults, errs...)
}

// SetIssueField sets one field of an issue as someone editing it in
// YouTrack would ("" clears it).
func (f *FakeYouTrack) SetIssueField(issueID, name, value string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i := f.issueLocked(issueID); i != nil {
		setFakeField(i, name, value)
	}
}

// IssueFields returns the issue's summary, description and field values,
// or false for an unknown issue.
func (f *FakeYouTrack) IssueFields(issueID string) (ytwrite.IssueSnapshot, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.issueLocked(issueID)
	if i == nil {
		return ytwrite.IssueSnapshot{}, false
	}
	return f.snapshotOf(i), true
}

func (f *FakeYouTrack) issueLocked(issueID string) *fakeIssue {
	for _, issues := range f.projects {
		for _, i := range issues {
			if i.ID == issueID {
				return i
			}
		}
	}
	return nil
}

func setFakeField(i *fakeIssue, name, value string) {
	if i.fields == nil {
		i.fields = map[string]string{}
	}
	if value == "" {
		delete(i.fields, name)
		return
	}
	i.fields[name] = value
}

func (f *FakeYouTrack) snapshotOf(i *fakeIssue) ytwrite.IssueSnapshot {
	s := ytwrite.IssueSnapshot{Summary: i.Summary, Description: i.Description, Fields: []ytwrite.FieldValue{}}
	for name, v := range i.fields {
		fv := ytwrite.FieldValue{Name: name, Value: v}
		if _, ok := f.bundles[name]; ok {
			fv.Type = ytwrite.FieldTypeEnum
		}
		s.Fields = append(s.Fields, fv)
	}
	return s
}

// UpdateIssueFields applies the snapshot to the issue, failing as the next
// queued Fault says (as CreateComment does). An unknown issue is a 404; a
// value outside a SetBundle field's values is a 400 and changes nothing.
func (f *FakeYouTrack) UpdateIssueFields(ctx context.Context, in ytwrite.IssueFieldsUpdate) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.updates = append(f.updates, in)
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var fault Fault
	if len(f.faults) > 0 {
		fault, f.faults = f.faults[0], f.faults[1:]
	}
	i := f.issueLocked(in.IssueID)
	if i == nil {
		return "", &ytwrite.RemoteError{Status: http.StatusNotFound, Message: "issue not found"}
	}
	for _, fv := range in.Fields {
		if set, ok := f.bundles[fv.Name]; ok && fv.Value != "" && !set[fv.Value] {
			return "", &ytwrite.RemoteError{Status: http.StatusBadRequest, Message: fmt.Sprintf("field %s has no value %s", fv.Name, fv.Value)}
		}
	}
	commit := func() string {
		f.commits++
		i.Summary, i.Description = in.Summary, in.Description
		for _, fv := range in.Fields {
			setFakeField(i, fv.Name, fv.Value)
		}
		return i.ID
	}
	switch fault.Kind {
	case CommitDropReply:
		commit()
		return "", errConnReset
	case NoID:
		commit()
		return "", nil
	case RateLimit:
		return "", &ytwrite.RemoteError{Status: http.StatusTooManyRequests, RetryAfter: fault.RetryAfter, Message: "rate limited"}
	case ServerError:
		if fault.Commit {
			commit()
		}
		st := fault.Status
		if st == 0 {
			st = http.StatusBadGateway
		}
		return "", &ytwrite.RemoteError{Status: st, Message: "server error"}
	case Refuse:
		return "", &ytwrite.RemoteError{Status: fault.Status, Message: "refused"}
	case DelayCommit:
		panic("ytwritetest: DelayCommit is not supported for issue fields")
	}
	return commit(), nil
}

// ReadIssueFields returns the issue as it is now; an unknown issue is a 404.
func (f *FakeYouTrack) ReadIssueFields(ctx context.Context, issueID string) (ytwrite.IssueSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if err := ctx.Err(); err != nil {
		return ytwrite.IssueSnapshot{}, err
	}
	if len(f.readFaults) > 0 {
		err := f.readFaults[0]
		f.readFaults = f.readFaults[1:]
		return ytwrite.IssueSnapshot{}, err
	}
	i := f.issueLocked(issueID)
	if i == nil {
		return ytwrite.IssueSnapshot{}, &ytwrite.RemoteError{Status: http.StatusNotFound, Message: "issue not found"}
	}
	return f.snapshotOf(i), nil
}
