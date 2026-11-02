package api

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

const LinearIssueHighWatermark = 230

var ErrLinearCapacity = errors.New("linear_capacity_pending")
var ErrLinearMirrorExcluded = errors.New("linear_mirror_excluded")

// Every production issue create (board, decisions and explicit requests)
// crosses this one client boundary. Tests can use a fake serializing gate.
type LinearCreateAdmission interface {
	SerializeLinearCreate(context.Context, func(store.LinearCreateLease) error) error
}

const admissionExistingQuery = `query HkAdmissionExisting($id:ID!){issues(first:2,includeArchived:true,filter:{id:{eq:$id}}){nodes{id identifier url} pageInfo{hasNextPage}}}`
const admissionCountQuery = `query HkAdmissionCount{issues(first:230,includeArchived:false){nodes{id} pageInfo{hasNextPage}}}`

func (a LinearAPI) admittedCreate(ctx context.Context, in LinearDecisionIssue) (LinearIssue, error) {
	if a.Admission == nil || !a.CapacityScopeVerified {
		return LinearIssue{}, fmt.Errorf("%w: admission unavailable", ErrLinearCapacity)
	}
	if in.ID == "" || in.TaskID < 1 {
		return LinearIssue{}, errors.New("linear create requires stable issue id")
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	var result LinearIssue
	err := a.Admission.SerializeLinearCreate(ctx, func(lease store.LinearCreateLease) error {
		// A lost response may have used the last slot. Recover before capacity
		// checks, including when the recovered issue has since been archived.
		existing, found, err := a.admissionExisting(ctx, in.ID)
		if err != nil {
			return fmt.Errorf("%w: identity read unavailable: %v", ErrLinearCapacity, err)
		}
		if found {
			result = existing
			return lease.Complete(ctx, in.ID)
		}
		pending, err := lease.Pending(ctx)
		if err != nil {
			return err
		}
		for _, id := range pending {
			_, found, err := a.admissionExisting(ctx, id)
			if err != nil || !found {
				return fmt.Errorf("%w: uncertain earlier create requires reconciliation", ErrLinearCapacity)
			}
			if err := lease.Complete(ctx, id); err != nil {
				return err
			}
		}
		// Lock acquisition and HTTP recovery may have waited. Re-read the hk
		// policy through this same connection immediately before admission.
		task, found, err := lease.Task(ctx, in.TaskID)
		if err != nil {
			return err
		}
		if !found || store.LinearMirrorReason(task) == "" {
			return ErrLinearMirrorExcluded
		}
		if in.RequestID != "" && (task.Refs.DecisionRequest == nil || task.Refs.DecisionRequest.ID != in.RequestID || task.Refs.DecisionRequest.Status != store.DecisionRequestOpen) {
			return ErrLinearMirrorExcluded
		}
		if err := a.admissionCapacity(ctx); err != nil {
			return err
		}
		if err := lease.Reserve(ctx, in.ID); err != nil {
			return err
		}
		result, err = a.createLinearIssue(ctx, in)
		if err != nil {
			return err
		} // durable reservation survives ambiguity
		return lease.Complete(ctx, in.ID)
	})
	return result, err
}

func (a LinearAPI) admissionExisting(ctx context.Context, id string) (LinearIssue, bool, error) {
	var out struct {
		Issues *struct {
			Nodes    []struct{ ID, Identifier, URL string }
			PageInfo *struct{ HasNextPage *bool }
		}
	}
	if err := a.graphql(ctx, linearWriteTimeout, admissionExistingQuery, map[string]any{"id": id}, &out); err != nil {
		return LinearIssue{}, false, err
	}
	if out.Issues == nil || out.Issues.Nodes == nil || out.Issues.PageInfo == nil || out.Issues.PageInfo.HasNextPage == nil || *out.Issues.PageInfo.HasNextPage || len(out.Issues.Nodes) > 1 {
		return LinearIssue{}, false, errors.New("incomplete identity result")
	}
	if len(out.Issues.Nodes) == 0 {
		return LinearIssue{}, false, nil
	}
	issue := out.Issues.Nodes[0]
	if issue.ID != id || issue.Identifier == "" || issue.URL == "" {
		return LinearIssue{}, false, errors.New("invalid identity result")
	}
	return LinearIssue{Identifier: issue.Identifier, URL: issue.URL}, true, nil
}

func (a LinearAPI) admissionCapacity(ctx context.Context) error {
	var out struct {
		Issues *struct {
			Nodes    []struct{ ID string }
			PageInfo *struct{ HasNextPage *bool }
		}
	}
	if err := a.graphql(ctx, linearWriteTimeout, admissionCountQuery, nil, &out); err != nil {
		return fmt.Errorf("%w: count unavailable: %v", ErrLinearCapacity, err)
	}
	if out.Issues == nil || out.Issues.Nodes == nil || out.Issues.PageInfo == nil || out.Issues.PageInfo.HasNextPage == nil {
		return fmt.Errorf("%w: incomplete count", ErrLinearCapacity)
	}
	seen := map[string]bool{}
	for _, issue := range out.Issues.Nodes {
		if issue.ID == "" || seen[issue.ID] {
			return fmt.Errorf("%w: invalid count", ErrLinearCapacity)
		}
		seen[issue.ID] = true
	}
	if len(seen) >= LinearIssueHighWatermark || *out.Issues.PageInfo.HasNextPage {
		return fmt.Errorf("%w: watermark reached", ErrLinearCapacity)
	}
	return nil
}
