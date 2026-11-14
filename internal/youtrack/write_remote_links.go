package youtrack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

var _ ytwrite.IssueLinksRemote = (*WriteRemote)(nil)

// The issue-link surface of WriteRemote (hk 3841, hk 4636 S1;
// design/2026-11-13/yt-issue-links §5.4 and §6). Every call is one request per
// page and none retries: a 401 or 429 comes back as a *ytwrite.RemoteError
// with the server's wait, as for the other kinds.
//
// A read pages with $skip by the rows it received and stops on an empty page,
// as ytwrite.EnumerateStableComments does. A "short page ends it" rule would
// read a server that caps its pages below $top as the end of the list and drop
// links silently. The linked issues of a row are paged the same way with
// $skipLinks.

const (
	// linkPageTop is the page a read asks for, for rows and for the linked
	// issues of a row.
	linkPageTop = 100
	// linkRowFields names every attribute a read uses; YouTrack returns only
	// what is asked for.
	linkRowFields = "id,direction,linkType(id),issues(id)"
)

// linkRow is one IssueLink as /links returns it: one per (link type,
// direction) of the issue, with its linked issues, empty ones included.
type linkRow struct {
	ID        string `json:"id"`
	Direction string `json:"direction"`
	LinkType  *struct {
		ID string `json:"id"`
	} `json:"linkType"`
	Issues []struct {
		ID string `json:"id"`
	} `json:"issues"`
}

// key identifies the row across pages. The id is the row's own; a row without
// one is named by what makes it unique on an issue.
func (row linkRow) key() string {
	if row.ID != "" {
		return row.ID
	}
	if row.LinkType == nil {
		return "/" + row.Direction
	}
	return row.LinkType.ID + "/" + row.Direction
}

// ReadIssueLinks reads the instance's link types, then every row of the
// issue's links, and returns the links on the side hk writes. The side comes
// from the row's direction field and the link type's role names, never from
// the text of an id. A type the instance does not configure as hk needs is a
// *ytwrite.RefusedError (ReasonFieldRefused) and no link row is read. A read
// that passes ytwrite.MaxEnumeratedLinks rows or linked issues is
// ytwrite.ErrEnumerationTooLarge: no partial result.
func (r *WriteRemote) ReadIssueLinks(ctx context.Context, issueID string) (ytwrite.IssueLinksRead, error) {
	types, err := r.readLinkTypes(ctx)
	if err != nil {
		return ytwrite.IssueLinksRead{}, err
	}
	links, err := r.readLinkRows(ctx, issueID, types)
	if err != nil {
		return ytwrite.IssueLinksRead{}, err
	}
	return ytwrite.IssueLinksRead{Types: types, Links: links}, nil
}

func (r *WriteRemote) getJSON(ctx context.Context, path string, query url.Values, out any) error {
	data, readErr, err := r.send(ctx, http.MethodGet, path, query, nil)
	if err != nil {
		return err
	}
	if readErr != nil {
		return readErr
	}
	if err := json.Unmarshal(data, out); err != nil {
		return decodeReplyErr("youtrack GET "+path, err)
	}
	return nil
}

func (r *WriteRemote) readLinkTypes(ctx context.Context) (ytwrite.LinkTypes, error) {
	const path = "/api/issueLinkTypes"
	var listed []ytwrite.RemoteLinkType
	seen := map[string]bool{}
	for skip := 0; ; {
		q := url.Values{}
		q.Set("fields", "id,name,directed,sourceToTarget,targetToSource")
		q.Set("$top", strconv.Itoa(linkPageTop))
		q.Set("$skip", strconv.Itoa(skip))
		var page []LinkType
		if err := r.getJSON(ctx, path, q, &page); err != nil {
			return ytwrite.LinkTypes{}, err
		}
		if len(page) == 0 {
			return ytwrite.ResolveLinkTypes(listed)
		}
		if len(listed)+len(page) > ytwrite.MaxEnumeratedLinks {
			return ytwrite.LinkTypes{}, fmt.Errorf("youtrack GET %s: more than %d link types: %w", path, ytwrite.MaxEnumeratedLinks, ytwrite.ErrEnumerationTooLarge)
		}
		for _, t := range page {
			if seen[t.ID] {
				return ytwrite.LinkTypes{}, fmt.Errorf("youtrack GET %s: link type %q on two pages: %w", path, t.ID, ytwrite.ErrInconsistentEnumeration)
			}
			seen[t.ID] = true
			listed = append(listed, ytwrite.RemoteLinkType{ID: t.ID, Name: t.Name, Directed: t.Directed, SourceToTarget: t.SourceToTarget, TargetToSource: t.TargetToSource})
		}
		skip += len(page)
	}
}

// linkEntry is one linked issue of one row.
type linkEntry struct{ row, issue string }

func (r *WriteRemote) readLinkRows(ctx context.Context, issueID string, types ytwrite.LinkTypes) ([]ytwrite.IssueLink, error) {
	path := "/api/issues/" + url.PathEscape(issueID) + "/links"
	tooLarge := func(what string) error {
		return fmt.Errorf("youtrack GET %s: more than %d %s: %w", path, ytwrite.MaxEnumeratedLinks, what, ytwrite.ErrEnumerationTooLarge)
	}
	ours := map[ytwrite.IssueLink]bool{}
	seenRows := map[string]bool{}
	seenEntries := map[linkEntry]bool{}
	rows := 0
	for skip := 0; ; {
		pageRows := -1
		// The rows of one page are asked for again with the next part of
		// their linked issues until a request shows no issue not seen before.
		// A server that ignores $skipLinks repeats the first part, which ends
		// the loop too.
		for skipLinks := 0; ; {
			q := url.Values{}
			q.Set("fields", linkRowFields)
			q.Set("$top", strconv.Itoa(linkPageTop))
			q.Set("$skip", strconv.Itoa(skip))
			q.Set("$topLinks", strconv.Itoa(linkPageTop))
			q.Set("$skipLinks", strconv.Itoa(skipLinks))
			var page []linkRow
			if err := r.getJSON(ctx, path, q, &page); err != nil {
				return nil, err
			}
			if pageRows < 0 {
				if len(page) == 0 {
					links := make([]ytwrite.IssueLink, 0, len(ours))
					for l := range ours {
						links = append(links, l)
					}
					ytwrite.SortLinks(links)
					return links, nil
				}
				pageRows = len(page)
				if rows+pageRows > ytwrite.MaxEnumeratedLinks {
					return nil, tooLarge("link rows")
				}
				rows += pageRows
				for _, row := range page {
					if seenRows[row.key()] {
						return nil, fmt.Errorf("youtrack GET %s: link row %q on two pages: %w", path, row.key(), ytwrite.ErrInconsistentEnumeration)
					}
					seenRows[row.key()] = true
				}
			}
			fresh, widest := 0, 0
			for _, row := range page {
				kind, isOurs, err := rowKind(types, row)
				if err != nil {
					return nil, fmt.Errorf("youtrack GET %s: %w", path, err)
				}
				widest = max(widest, len(row.Issues))
				for _, issue := range row.Issues {
					entry := linkEntry{row.key(), issue.ID}
					if seenEntries[entry] {
						continue
					}
					seenEntries[entry] = true
					fresh++
					if len(seenEntries) > ytwrite.MaxEnumeratedLinks {
						return nil, tooLarge("linked issues")
					}
					if !isOurs {
						continue
					}
					if issue.ID == "" {
						return nil, decodeReplyErr("youtrack GET "+path, errors.New("a linked issue has no id"))
					}
					ours[ytwrite.IssueLink{Kind: kind, Target: issue.ID}] = true
				}
			}
			if fresh == 0 {
				break
			}
			skipLinks += widest
		}
		skip += pageRows
	}
}

// rowKind classifies a row by its link type id and its direction field. A row
// without a link type (the field can be null) is no managed kind.
func rowKind(types ytwrite.LinkTypes, row linkRow) (ytwrite.LinkKind, bool, error) {
	if row.LinkType == nil {
		return "", false, nil
	}
	return types.Classify(row.LinkType.ID, ytwrite.LinkDirection(row.Direction))
}

// linkURLID is the {linkID} URL segment for a link from the issue's writer
// side, or an error for a link that names no managed kind or no target.
func linkURLID(types ytwrite.LinkTypes, link ytwrite.IssueLink) (string, error) {
	managed, ok := types.Of(link.Kind)
	if !ok {
		return "", fmt.Errorf("%w: no managed link type for kind %q", ytwrite.ErrInvalid, link.Kind)
	}
	if link.Target == "" {
		return "", fmt.Errorf("%w: a %s link names no target issue", ytwrite.ErrInvalid, link.Kind)
	}
	return managed.LinkID(), nil
}

// AddIssueLink makes one POST /api/issues/{id}/links/{linkID}/issues with the
// target's id. The reply is not read: a 2xx is the add.
func (r *WriteRemote) AddIssueLink(ctx context.Context, issueID string, types ytwrite.LinkTypes, link ytwrite.IssueLink) error {
	linkID, err := linkURLID(types, link)
	if err != nil {
		return err
	}
	q := url.Values{}
	q.Set("fields", "id")
	_, _, err = r.send(ctx, http.MethodPost, "/api/issues/"+url.PathEscape(issueID)+"/links/"+url.PathEscape(linkID)+"/issues", q, map[string]string{"id": link.Target})
	return err
}

// RemoveIssueLink makes one DELETE /api/issues/{id}/links/{linkID}/issues/{target}.
// A 404 may mean the link is already gone, or that the issue or the type is: the
// links are re-read, and the link absent is success. The link still present is
// the 404 itself, and a re-read that fails is that failure (never success).
func (r *WriteRemote) RemoveIssueLink(ctx context.Context, issueID string, types ytwrite.LinkTypes, link ytwrite.IssueLink) error {
	linkID, err := linkURLID(types, link)
	if err != nil {
		return err
	}
	q := url.Values{}
	q.Set("fields", "id")
	_, _, err = r.send(ctx, http.MethodDelete, "/api/issues/"+url.PathEscape(issueID)+"/links/"+url.PathEscape(linkID)+"/issues/"+url.PathEscape(link.Target), q, nil)
	var remote *ytwrite.RemoteError
	if !errors.As(err, &remote) || remote.Status != http.StatusNotFound {
		return err
	}
	links, readErr := r.readLinkRows(ctx, issueID, types)
	if readErr != nil {
		return readErr
	}
	for _, l := range links {
		if l == link {
			return err
		}
	}
	return nil
}
