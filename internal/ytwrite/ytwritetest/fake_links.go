package ytwritetest

import (
	"context"
	"sort"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// SetIssueLinks seeds the current hk-owned link state of an issue.
func (f *FakeYouTrack) SetIssueLinks(issueID string, links ytwrite.IssueLinks) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.issueLinks[issueID] = cloneLinks(links)
}

// IssueLinkWrites returns the issue-link snapshots received, in order.
func (f *FakeYouTrack) IssueLinkWrites() []FakeIssueLinksWrite {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]FakeIssueLinksWrite, len(f.linkWrites))
	for i, w := range f.linkWrites {
		out[i] = FakeIssueLinksWrite{IssueID: w.IssueID, Links: cloneLinks(w.Links)}
	}
	return out
}

func (f *FakeYouTrack) UpdateIssueLinks(ctx context.Context, issueID string, snapshot ytwrite.IssueLinksSnapshot) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	current := cloneLinks(f.issueLinks[issueID])
	if current.Parent != "" && current.Parent != snapshot.Desired.Parent && current.Parent != snapshot.Previous.Parent {
		return "", &ytwrite.RefusedError{Reason: ytwrite.ReasonUnownedLink, Detail: "unowned parent link to " + current.Parent}
	}
	if id := unexpectedFakeLink(current.Related, snapshot.Previous.Related, snapshot.Desired.Related); id != "" {
		return "", &ytwrite.RefusedError{Reason: ytwrite.ReasonUnownedLink, Detail: "unowned relates link to " + id}
	}
	if id := unexpectedFakeLink(current.Duplicates, snapshot.Previous.Duplicates, snapshot.Desired.Duplicates); id != "" {
		return "", &ytwrite.RefusedError{Reason: ytwrite.ReasonUnownedLink, Detail: "unowned duplicate link to " + id}
	}
	if snapshot.Desired.Parent != "" {
		current.Parent = snapshot.Desired.Parent
	} else if current.Parent == snapshot.Previous.Parent {
		current.Parent = ""
	}
	current.Related = updateFakeLinkSet(current.Related, snapshot.Previous.Related, snapshot.Desired.Related)
	current.Duplicates = updateFakeLinkSet(current.Duplicates, snapshot.Previous.Duplicates, snapshot.Desired.Duplicates)
	f.issueLinks[issueID] = current
	f.linkWrites = append(f.linkWrites, FakeIssueLinksWrite{IssueID: issueID, Links: cloneLinks(snapshot.Desired)})
	return issueID, nil
}

func unexpectedFakeLink(current, previous, desired []string) string {
	for _, id := range current {
		if !containsFakeLink(desired, id) && !containsFakeLink(previous, id) {
			return id
		}
	}
	return ""
}

func updateFakeLinkSet(current, previous, desired []string) []string {
	out := make([]string, 0, len(current)+len(desired))
	for _, id := range current {
		if containsFakeLink(previous, id) && !containsFakeLink(desired, id) {
			continue
		}
		out = append(out, id)
	}
	for _, id := range desired {
		if !containsFakeLink(out, id) {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

func containsFakeLink(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

func (f *FakeYouTrack) ReadIssueLinks(ctx context.Context, issueID string) (ytwrite.IssueLinks, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return ytwrite.IssueLinks{}, err
	}
	return cloneLinks(f.issueLinks[issueID]), nil
}

func cloneLinks(in ytwrite.IssueLinks) ytwrite.IssueLinks {
	out := ytwrite.IssueLinks{Parent: in.Parent, Related: append([]string(nil), in.Related...), Duplicates: append([]string(nil), in.Duplicates...)}
	sort.Strings(out.Related)
	sort.Strings(out.Duplicates)
	if out.Related == nil {
		out.Related = []string{}
	}
	if out.Duplicates == nil {
		out.Duplicates = []string{}
	}
	return out
}
