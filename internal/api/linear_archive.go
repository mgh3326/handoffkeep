package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

const linearIssueLowWatermark = 200
const linearArchiveRecentWindow = 7 * 24 * time.Hour

const archiveCandidatesQuery = `query HkArchiveCandidates($after:String){issues(first:100,after:$after,includeArchived:false,filter:{state:{type:{in:["completed","canceled"]}}}){nodes{id completedAt canceledAt}pageInfo{hasNextPage endCursor}}}`
const archiveSnapshotQuery = `query HkArchiveSnapshot($id:String!){issue(id:$id){id identifier title description url createdAt updatedAt completedAt canceledAt archivedAt priority dueDate state{id name type}team{id key}assignee{id name}delegate{id name}parent{id identifier}children(first:1){nodes{id}pageInfo{hasNextPage}}labels(first:100){nodes{id name}pageInfo{hasNextPage}}comments(first:100){nodes{id body createdAt updatedAt user{id name}}pageInfo{hasNextPage}}relations(first:100){nodes{id type issue{id identifier}relatedIssue{id identifier}}pageInfo{hasNextPage}}inverseRelations(first:100){nodes{id type issue{id identifier}relatedIssue{id identifier}}pageInfo{hasNextPage}}attachments(first:100){nodes{id title url}pageInfo{hasNextPage}}}}`
const archiveMutation = `mutation HkWatermarkArchive($id:String!){issueArchive(id:$id,trash:false){success}}`
const archiveReceiptQuery = `query HkArchiveReceipt($id:String!){issue(id:$id){id archivedAt}}`

type archiveConnection struct {
	Nodes    []json.RawMessage
	PageInfo *struct{ HasNextPage *bool }
}

func (c archiveConnection) complete() bool {
	return c.Nodes != nil && c.PageInfo != nil && c.PageInfo.HasNextPage != nil && !*c.PageInfo.HasNextPage
}

type archiveSnapshot struct {
	ID, Identifier, UpdatedAt, CompletedAt, CanceledAt, ArchivedAt       string
	State                                                                struct{ Type string }
	Parent                                                               *struct{ ID string }
	Children, Labels, Comments, Relations, InverseRelations, Attachments archiveConnection
}

func (s archiveSnapshot) eligible(cutoff time.Time) bool {
	if s.ID == "" || s.Identifier == "" || s.ArchivedAt != "" || (s.State.Type != "completed" && s.State.Type != "canceled") {
		return false
	}
	updated, err := time.Parse(time.RFC3339Nano, s.UpdatedAt)
	if err != nil || !updated.Before(cutoff) {
		return false
	}
	closed := s.CompletedAt
	if s.State.Type == "canceled" {
		closed = s.CanceledAt
	}
	closedAt, err := time.Parse(time.RFC3339Nano, closed)
	if err != nil || !closedAt.Before(cutoff) {
		return false
	}
	for _, c := range []archiveConnection{s.Children, s.Labels, s.Comments, s.Relations, s.InverseRelations, s.Attachments} {
		if !c.complete() {
			return false
		}
	}
	// Avoid unverified parent/sub-issue cascade behavior. Dots labels are
	// conservative protection until its ongoing-request model is available.
	if s.Parent != nil || len(s.Children.Nodes) > 0 {
		return false
	}
	for _, raw := range s.Labels.Nodes {
		var label struct{ Name string }
		if json.Unmarshal(raw, &label) != nil || label.Name == "" {
			return false
		}
		if strings.EqualFold(label.Name, "keep") || strings.EqualFold(label.Name, store.DotsLabel) {
			return false
		}
	}
	for _, raw := range s.Comments.Nodes {
		var comment struct{ ID, UpdatedAt string }
		if json.Unmarshal(raw, &comment) != nil || comment.ID == "" {
			return false
		}
		at, err := time.Parse(time.RFC3339Nano, comment.UpdatedAt)
		if err != nil || !at.Before(cutoff) {
			return false
		}
	}
	return true
}

type archiveCandidate struct{ ID, CompletedAt, CanceledAt string }

func (c archiveCandidate) closedAt() time.Time {
	value := c.CompletedAt
	if value == "" {
		value = c.CanceledAt
	}
	at, _ := time.Parse(time.RFC3339Nano, value)
	return at
}
func (a LinearAPI) archiveCandidates(ctx context.Context) ([]archiveCandidate, error) {
	candidates := []archiveCandidate{}
	after := ""
	seen := map[string]bool{}
	for {
		var out struct {
			Issues *struct {
				Nodes    []archiveCandidate
				PageInfo *struct {
					HasNextPage *bool
					EndCursor   string
				}
			}
		}
		if err := a.graphql(ctx, linearWriteTimeout, archiveCandidatesQuery, map[string]any{"after": archiveCursor(after)}, &out); err != nil {
			return nil, err
		}
		if out.Issues == nil || out.Issues.Nodes == nil || out.Issues.PageInfo == nil || out.Issues.PageInfo.HasNextPage == nil {
			return nil, errors.New("incomplete archive candidates")
		}
		for _, c := range out.Issues.Nodes {
			if c.ID == "" || seen[c.ID] {
				return nil, errors.New("invalid archive candidate identity")
			}
			seen[c.ID] = true
			candidates = append(candidates, c)
		}
		page := out.Issues.PageInfo
		if !*page.HasNextPage {
			break
		}
		if page.EndCursor == "" || page.EndCursor == after || len(out.Issues.Nodes) == 0 {
			return nil, errors.New("non-advancing archive cursor")
		}
		after = page.EndCursor
	}
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i].closedAt(), candidates[j].closedAt()
		if a.Equal(b) {
			return candidates[i].ID < candidates[j].ID
		}
		return a.Before(b)
	})
	return candidates, nil
}
func (a LinearAPI) archiveSnapshot(ctx context.Context, id string) (archiveSnapshot, []byte, error) {
	var out struct{ Issue json.RawMessage }
	if err := a.graphql(ctx, linearWriteTimeout, archiveSnapshotQuery, map[string]any{"id": id}, &out); err != nil {
		return archiveSnapshot{}, nil, err
	}
	var snapshot archiveSnapshot
	if err := json.Unmarshal(out.Issue, &snapshot); err != nil {
		return snapshot, nil, err
	}
	if snapshot.ID != id {
		return snapshot, nil, errors.New("archive snapshot identity mismatch")
	}
	var canonical any
	if err := json.Unmarshal(out.Issue, &canonical); err != nil {
		return snapshot, nil, err
	}
	body, err := json.Marshal(canonical)
	return snapshot, body, err
}

func (a LinearAPI) archiveAndVerify(ctx context.Context, id string) error {
	var out struct{ IssueArchive struct{ Success bool } }
	mutationErr := a.graphql(ctx, linearWriteTimeout, archiveMutation, map[string]any{"id": id}, &out)
	var receipt struct {
		Issue *struct{ ID, ArchivedAt string }
	}
	readErr := a.graphql(ctx, linearWriteTimeout, archiveReceiptQuery, map[string]any{"id": id}, &receipt)
	if readErr == nil && receipt.Issue != nil && receipt.Issue.ID == id {
		if at, err := time.Parse(time.RFC3339Nano, receipt.Issue.ArchivedAt); err == nil && !at.IsZero() {
			return nil
		}
	}
	if mutationErr != nil {
		return mutationErr
	}
	if readErr != nil {
		return readErr
	}
	return errors.New("archive readback not confirmed")
}

// ensureLinearCapacity runs only while the common creation lease is held.
// A durable drain latch resumes interrupted 230->200 cycles below 230.
func (a LinearAPI) ensureLinearCapacity(ctx context.Context, lease store.LinearCreateLease) error {
	count, err := a.activeIssueCount(ctx)
	if err != nil {
		return err
	}
	if !a.WatermarkArchiveEnabled {
		if count >= LinearIssueHighWatermark {
			return fmt.Errorf("%w: watermark reached", ErrLinearCapacity)
		}
		return nil
	}
	archive, ok := lease.(store.LinearArchiveLease)
	if !ok {
		return fmt.Errorf("%w: archive lease unavailable", ErrLinearCapacity)
	}
	draining, err := archive.ArchiveCycle(ctx, nil)
	if err != nil {
		return err
	}
	if count >= LinearIssueHighWatermark && !draining {
		set := true
		draining, err = archive.ArchiveCycle(ctx, &set)
		if err != nil {
			return err
		}
	}
	if !draining {
		return nil
	}
	if count <= linearIssueLowWatermark {
		set := false
		_, err = archive.ArchiveCycle(ctx, &set)
		return err
	}
	candidates, err := a.archiveCandidates(ctx)
	if err != nil {
		return err
	}
	cutoff := time.Now().UTC().Add(-linearArchiveRecentWindow)
	for _, candidate := range candidates {
		snapshot, body, err := a.archiveSnapshot(ctx, candidate.ID)
		if err != nil {
			return err
		}
		if !snapshot.eligible(cutoff) {
			continue
		}
		key, err := archive.CopyArchive(ctx, candidate.ID, string(body))
		if err != nil {
			return err
		}
		archived, err := archive.ArchiveIfUnprotected(ctx, candidate.ID, snapshot.Identifier, cutoff, func() error {
			current, currentBody, err := a.archiveSnapshot(ctx, candidate.ID)
			if err != nil {
				return err
			}
			if !current.eligible(cutoff) || !bytes.Equal(body, currentBody) {
				return errArchiveChanged
			}
			return a.archiveAndVerify(ctx, candidate.ID)
		})
		if errors.Is(err, errArchiveChanged) {
			continue
		}
		if err != nil {
			return err
		}
		if !archived {
			continue
		}
		log.Printf("linear_archive.archived issue=%q copy=%q", snapshot.Identifier, key)
		count, err = a.activeIssueCount(ctx)
		if err != nil {
			return err
		}
		if count <= linearIssueLowWatermark {
			set := false
			_, err = archive.ArchiveCycle(ctx, &set)
			return err
		}
	}
	return fmt.Errorf("%w: protected work prevents watermark drain", ErrLinearCapacity)
}

var errArchiveChanged = errors.New("archive candidate changed after copy")

// A successful board/state update remains successful even if subsequent
// archival is pending. Before-create admission will resume any drain later.
func (a LinearAPI) checkCapacityAfterChange(ctx context.Context) {
	if a.Admission == nil || !a.CapacityScopeVerified {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	err := a.Admission.SerializeLinearCreate(ctx, func(lease store.LinearCreateLease) error { return a.ensureLinearCapacity(ctx, lease) })
	if err != nil {
		log.Printf("linear_archive.pending err=%q", err)
	}
}

func archiveCursor(value string) any {
	if value == "" {
		return nil
	}
	return value
}
