package api

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

const LinearIssueHighWatermark = 230
const LinearIssueCreateCeiling = 270

var ErrLinearCapacity = errors.New("linear_capacity_pending")
var ErrLinearMirrorExcluded = errors.New("linear_mirror_excluded")

// Every production issue create (board, decisions and explicit requests)
// crosses this one client boundary. Tests can use a fake serializing gate.
type LinearCreateAdmission interface {
	SerializeLinearCreate(context.Context, func(store.LinearCreateLease) error) error
}

const admissionExistingQuery = `query HkAdmissionExisting($id:ID!){issues(first:2,includeArchived:true,filter:{id:{eq:$id}}){nodes{id identifier url} pageInfo{hasNextPage}}}`
const admissionCountQuery = `query HkAdmissionCount($after:String){issues(first:100,after:$after,includeArchived:false){nodes{id} pageInfo{hasNextPage endCursor}}}`

func (a LinearAPI) admittedCreate(ctx context.Context, in LinearDecisionIssue) (LinearIssue, error) {
	if in.Board {
		return a.admittedBoardCreate(ctx, in)
	}
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
			board, err := lease.LinearBoardReservation(ctx, id)
			if err != nil {
				return err
			}
			if board != store.LinearBoardReservationNone {
				continue
			} // its slot is booked until webhook/manual repair

			_, found, err := a.admissionExisting(ctx, id)
			if err != nil {
				return fmt.Errorf("%w: earlier create readback unavailable", ErrLinearCapacity)
			}
			if !found {
				rejected, err := lease.RejectionKnown(ctx, id)
				if err != nil {
					return err
				}
				if !rejected {
					if id == in.ID {
						return fmt.Errorf("%w: repair this ambiguous reservation before retry", ErrLinearCapacity)
					}
					continue // book the unknown outcome; do not stop unrelated creates
				}
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
		if err := a.admissionCapacity(ctx, lease); err != nil {
			return err
		}
		if err := lease.Reserve(ctx, in.ID); err != nil {
			return err
		}
		result, err = a.createLinearIssue(ctx, in)
		if err != nil {
			var rejection linearRejectionError
			var holdErr error
			if errors.As(err, &rejection) && rejection.capacity {
				holdErr = lease.HoldCapacity(ctx, a.CapacityPolicy.Limit())
			}
			var rate linearRateLimitError
			if (errors.As(err, &rejection) && rejection.definite) || errors.As(err, &rate) || errors.Is(err, ErrLinearRateLimited) {
				return errors.Join(err, holdErr, lease.Complete(ctx, in.ID))
			}
			// Non-board producers already use exact-id readback. Only a known
			// issue completes ambiguity; absence retains its booked slot and
			// can be released by explicit, grace-bounded reservation repair.
			recovered, found, readErr := a.admissionExisting(ctx, in.ID)
			if readErr == nil && found {
				result = recovered
				return errors.Join(holdErr, lease.Complete(ctx, in.ID))
			}
			return errors.Join(err, holdErr)
		}
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

func (a LinearAPI) activeIssueCount(ctx context.Context) (int, error) {
	seen := map[string]bool{}
	after := ""
	for {
		var out struct {
			Issues *struct {
				Nodes    []struct{ ID string }
				PageInfo *struct {
					HasNextPage *bool
					EndCursor   string
				}
			}
		}
		var cursor any
		if after != "" {
			cursor = after
		}
		if err := a.graphql(ctx, linearWriteTimeout, admissionCountQuery, map[string]any{"after": cursor}, &out); err != nil {
			return 0, fmt.Errorf("%w: count unavailable: %v", ErrLinearCapacity, err)
		}
		if out.Issues == nil || out.Issues.Nodes == nil || out.Issues.PageInfo == nil || out.Issues.PageInfo.HasNextPage == nil {
			return 0, fmt.Errorf("%w: incomplete count", ErrLinearCapacity)
		}
		for _, issue := range out.Issues.Nodes {
			if issue.ID == "" || seen[issue.ID] {
				return 0, fmt.Errorf("%w: invalid count", ErrLinearCapacity)
			}
			seen[issue.ID] = true
		}
		if len(seen) >= a.CapacityPolicy.CreateCeiling() {
			return a.CapacityPolicy.CreateCeiling(), nil
		}
		page := out.Issues.PageInfo
		if !*page.HasNextPage {
			return len(seen), nil
		}
		if page.EndCursor == "" || page.EndCursor == after || len(out.Issues.Nodes) == 0 {
			return 0, fmt.Errorf("%w: non-advancing count cursor", ErrLinearCapacity)
		}
		after = page.EndCursor
	}
}

func (a LinearAPI) checkCapacityHold(ctx context.Context, lease store.LinearCreateLease, count int) error {
	priorLimit, held, err := lease.CapacityHold(ctx)
	if err != nil {
		return err
	}
	if !held {
		return nil
	}
	policy := a.CapacityPolicy
	if count <= policy.ArchiveLow() || (policy.Limit() < priorLimit && count < policy.CreateCeiling()) {
		return lease.ClearCapacityHold(ctx)
	}
	return fmt.Errorf("%w: vendor rejected issue capacity", ErrLinearCapacity)
}
func (a LinearAPI) admissionCapacity(ctx context.Context, lease store.LinearCreateLease) error {
	count, err := a.activeIssueCount(ctx)
	if err != nil {
		return err
	}
	// Every unresolved outcome books a slot. This includes ambiguity and
	// accepted board issues awaiting webhooks; unrelated creates can continue
	// while verified capacity remains. Double-counting briefly is conservative.
	reserved, err := lease.ReservedCreates(ctx)
	if err != nil {
		return err
	}
	count += reserved
	if err := a.checkCapacityHold(ctx, lease, count); err != nil {
		return err
	}
	if count >= a.CapacityPolicy.CreateCeiling() {
		return fmt.Errorf("%w: create ceiling reached", ErrLinearCapacity)
	}
	return nil
}

// Normal board delivery never reads back an issue. Acceptance is linked by
// the signed webhook. Ambiguous outcomes book slots and have explicit repair.
func (a LinearAPI) admittedBoardCreate(ctx context.Context, in LinearDecisionIssue) (LinearIssue, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	if a.Admission == nil || !a.CapacityScopeVerified {
		return LinearIssue{}, ErrLinearCapacity
	}
	if in.TaskID < 1 || in.ID != store.LinearDecisionUUID(fmt.Sprintf("work-board:task:%d", in.TaskID)) {
		return LinearIssue{}, errors.New("invalid board create identity")
	}
	var result LinearIssue
	err := a.Admission.SerializeLinearCreate(ctx, func(lease store.LinearCreateLease) error {
		task, found, err := lease.Task(ctx, in.TaskID)
		if err != nil {
			return err
		}
		if !found || store.LinearMirrorReason(task) == "" {
			return ErrLinearMirrorExcluded
		}
		reservedBefore, err := lease.LinearBoardReservation(ctx, in.ID)
		if err != nil {
			return err
		}
		if reservedBefore != store.LinearBoardReservationNone {
			return boardReservationError(reservedBefore)
		}
		if err = a.admissionCapacity(ctx, lease); err != nil {
			return err
		}
		reserved, err := lease.ReserveBoard(ctx, in.ID, in.TaskID)
		if err != nil {
			return err
		}
		if !reserved {
			state, err := lease.LinearBoardReservation(ctx, in.ID)
			if err != nil {
				return err
			}
			return boardReservationError(state)
		}
		result, err = a.createLinearIssue(ctx, in)
		if err != nil {
			var rejection linearRejectionError
			var holdErr error
			if errors.As(err, &rejection) && rejection.capacity {
				holdErr = lease.HoldCapacity(ctx, a.CapacityPolicy.Limit())
			}
			// A received HTTP 4xx, explicit refused mutation or capacity
			// rejection did not create an issue. Release only that known
			// rejection; transport/5xx/ambiguous GraphQL retains identity.
			if errors.As(err, &rejection) && rejection.definite {
				return errors.Join(err, holdErr, lease.Complete(ctx, in.ID))
			}
			var rate linearRateLimitError
			if errors.As(err, &rate) || errors.Is(err, ErrLinearRateLimited) {
				return errors.Join(err, lease.Complete(ctx, in.ID))
			}
			return errors.Join(err, holdErr)
		}
		return lease.AwaitBoardWebhook(ctx, in.ID)
	})
	return result, err
}
