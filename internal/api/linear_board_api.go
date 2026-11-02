package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/mgh3326/handoffkeep/internal/store"
)

const boardTargetQuery = `query FleetBoardTarget {teams(first:2,filter:{key:{eq:"ROB"}}){nodes{id states(first:100){nodes{id name} pageInfo{hasNextPage}}}}}`

func (a LinearAPI) BoardTarget(ctx context.Context) (BoardTarget, error) {
	var out struct {
		Teams struct {
			Nodes []struct {
				ID     string
				States struct {
					Nodes    []struct{ ID, Name string }
					PageInfo struct{ HasNextPage bool }
				}
			}
		}
	}
	if err := a.graphql(ctx, linearWriteTimeout, boardTargetQuery, nil, &out); err != nil {
		return BoardTarget{}, err
	}
	if len(out.Teams.Nodes) != 1 || out.Teams.Nodes[0].States.PageInfo.HasNextPage {
		return BoardTarget{}, errors.New("ROB team or states incomplete")
	}
	team := out.Teams.Nodes[0]
	result := BoardTarget{TeamID: team.ID, States: map[string]string{}}
	for _, s := range team.States.Nodes {
		result.States[s.Name] = s.ID
	}
	return result, nil
}

const boardIssueQuery = `query FleetBoardIssue($id:String!){issue(id:$id){id identifier description team{id} labels(first:100){nodes{id name} pageInfo{hasNextPage}}}}`

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

const boardLabelsQuery = `query FleetBoardLabels($name:String!){issueLabels(first:100,filter:{name:{eq:$name}}){nodes{id team{id}} pageInfo{hasNextPage}}}`

func (a LinearAPI) findBoardLabel(ctx context.Context, teamID, name string) (string, error) {
	var out struct {
		IssueLabels struct {
			Nodes []struct {
				ID   string
				Team *struct{ ID string }
			}
			PageInfo struct{ HasNextPage bool }
		}
	}
	if err := a.graphql(ctx, linearWriteTimeout, boardLabelsQuery, map[string]any{"name": name}, &out); err != nil {
		return "", err
	}
	if out.IssueLabels.PageInfo.HasNextPage {
		return "", errors.New("linear board labels incomplete")
	}
	workspace := ""
	for _, label := range out.IssueLabels.Nodes {
		if label.Team != nil && label.Team.ID == teamID {
			return label.ID, nil
		}
		if label.Team == nil {
			workspace = label.ID
		}
	}
	return workspace, nil
}

const boardLabelCreateMutation = `mutation FleetBoardLabel($input:IssueLabelCreateInput!){issueLabelCreate(input:$input){success issueLabel{id}}}`

func (a LinearAPI) BoardLabel(ctx context.Context, teamID, name string) (string, error) {
	id, err := a.findBoardLabel(ctx, teamID, name)
	if err != nil || id != "" {
		return id, err
	}
	var out struct {
		IssueLabelCreate struct {
			Success    bool
			IssueLabel *struct{ ID string }
		}
	}
	input := map[string]any{"id": store.LinearDecisionUUID("work-board:label:" + teamID + ":" + name), "teamId": teamID, "name": name}
	err = a.graphql(ctx, linearWriteTimeout, boardLabelCreateMutation, map[string]any{"input": input}, &out)
	if err == nil && out.IssueLabelCreate.Success && out.IssueLabelCreate.IssueLabel != nil {
		return out.IssueLabelCreate.IssueLabel.ID, nil
	}
	// Another task may have created the same label, or the response was lost.
	id, readErr := a.findBoardLabel(ctx, teamID, name)
	if readErr == nil && id != "" {
		return id, nil
	}
	return "", fmt.Errorf("linear board label %q unavailable", name)
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
