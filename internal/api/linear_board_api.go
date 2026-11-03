package api

import (
	"context"
	"errors"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

func (a LinearAPI) BoardTarget(ctx context.Context) (BoardTarget, error) {
	return a.localBoardTarget(ctx)
}

const boardIssueQuery = `query FleetBoardIssue($id:String!){issue(id:$id){id identifier archivedAt description team{id} labels(first:100,includeArchived:true){nodes{id name} pageInfo{hasNextPage}}}}`

func (a LinearAPI) BoardIssue(ctx context.Context, id string) (BoardIssue, error) {
	var out struct {
		Issue *BoardIssue `json:"issue"`
	}
	if err := a.graphql(ctx, linearWriteTimeout, boardIssueQuery, map[string]any{"id": id}, &out); err != nil {
		return BoardIssue{}, err
	}
	if out.Issue == nil {
		return BoardIssue{}, errors.New("linear board issue not found")
	}
	return *out.Issue, nil
}

const boardLabelCreateMutation = `mutation FleetBoardLabel($input:IssueLabelCreateInput!){issueLabelCreate(input:$input){success issueLabel{id updatedAt}}}`
const boardLabelByIDQuery = `query HkBoardLabelByID($id:String!){issueLabel(id:$id){id name updatedAt team{id}}}`

func (a LinearAPI) BoardLabel(ctx context.Context, teamID, name string) (string, error) {
	if a.Metadata == nil {
		return "", errors.New("linear metadata store unavailable")
	}
	id, err := a.Metadata.ResolveLinearBoardLabel(ctx, teamID, name, func(ctx context.Context) (store.LinearMetadata, error) {
		stable := store.LinearDecisionUUID("work-board:label:" + teamID + ":" + name)
		var out struct {
			IssueLabelCreate struct {
				Success    *bool
				IssueLabel *struct {
					ID        string
					UpdatedAt time.Time
				}
			}
		}
		createErr := a.graphql(ctx, linearWriteTimeout, boardLabelCreateMutation, map[string]any{"input": map[string]any{"id": stable, "teamId": teamID, "name": name}}, &out)
		if createErr == nil && out.IssueLabelCreate.Success != nil && *out.IssueLabelCreate.Success && out.IssueLabelCreate.IssueLabel != nil && out.IssueLabelCreate.IssueLabel.ID == stable {
			return store.LinearMetadata{Kind: "label", ID: stable, TeamID: teamID, Name: name, UpdatedAt: out.IssueLabelCreate.IssueLabel.UpdatedAt}, nil
		}
		if createErr == nil {
			if out.IssueLabelCreate.Success != nil && !*out.IssueLabelCreate.Success {
				createErr = linearRejectionError{err: errors.New("linear label create refused"), definite: true, mutationRefused: true}
			} else {
				createErr = errors.New("linear label create response incomplete")
			}
		}
		var rate linearRateLimitError
		if errors.As(createErr, &rate) || errors.Is(createErr, ErrLinearRateLimited) {
			return store.LinearMetadata{}, createErr
		}
		// Exact id recovery after an uncertain create; never a content scan.
		var read struct {
			IssueLabel *struct {
				ID, Name  string
				UpdatedAt time.Time
				Team      *struct{ ID string }
			}
		}
		if err := a.graphql(ctx, linearWriteTimeout, boardLabelByIDQuery, map[string]any{"id": stable}, &read); err != nil {
			var rate linearRateLimitError
			if errors.As(err, &rate) || errors.Is(err, ErrLinearRateLimited) {
				return store.LinearMetadata{}, err
			}
			return store.LinearMetadata{}, err
		}
		if label := read.IssueLabel; label != nil && label.ID == stable && label.Name == name && label.Team != nil && label.Team.ID == teamID {
			return store.LinearMetadata{Kind: "label", ID: stable, TeamID: teamID, Name: name, UpdatedAt: label.UpdatedAt}, nil
		}
		var rejected linearRejectionError
		if !errors.As(createErr, &rejected) || !rejected.mutationRefused {
			return store.LinearMetadata{}, createErr
		}
		return store.LinearMetadata{}, store.ErrLinearBoardLabelUnavailable
	})
	if errors.Is(err, store.ErrLinearBoardLabelUnavailable) {
		return "", linearBoardLabelUnavailableError{Name: name, TeamID: teamID}
	}
	return id, err
}

func (a LinearAPI) UpdateBoardIssue(ctx context.Context, id, state, description string, labels []string) error {
	var out struct{ IssueUpdate struct{ Success bool } }
	input := map[string]any{"stateId": state, "description": description, "labelIds": labels}
	if err := a.graphql(ctx, linearWriteTimeout, linearIssueUpdateMutation, map[string]any{"id": id, "input": input}, &out); err != nil {
		return err
	}
	if !out.IssueUpdate.Success {
		return errors.New("linear board issue update refused")
	}
	return nil
}

const boardCommentQuery = `query FleetBoardComment($id:String!){comment(id:$id){id body issue{id}}}`

func (a LinearAPI) BoardCommentExists(ctx context.Context, id, issueID, body string) (bool, error) {
	var out struct {
		Comment *struct {
			ID, Body string
			Issue    struct{ ID string }
		}
	}
	if err := a.graphql(ctx, linearWriteTimeout, boardCommentQuery, map[string]any{"id": id}, &out); err != nil {
		return false, err
	}
	return out.Comment != nil && out.Comment.ID == id && out.Comment.Body == body && out.Comment.Issue.ID == issueID, nil
}
