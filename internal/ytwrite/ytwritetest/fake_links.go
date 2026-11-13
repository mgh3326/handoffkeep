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

func (f *FakeYouTrack) UpdateIssueLinks(ctx context.Context, issueID string, links ytwrite.IssueLinks) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	f.issueLinks[issueID] = cloneLinks(links)
	f.linkWrites = append(f.linkWrites, FakeIssueLinksWrite{IssueID: issueID, Links: cloneLinks(links)})
	return issueID, nil
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
