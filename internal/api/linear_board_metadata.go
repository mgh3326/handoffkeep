package api

import (
	"context"
	"errors"
	"fmt"
	"github.com/mgh3326/handoffkeep/internal/store"
	"time"
)

type LinearBoardMetadataStore interface {
	EnsureLinearBoardMetadata(context.Context, func(context.Context) ([]store.LinearMetadata, error)) error
	RefreshLinearBoardMetadata(context.Context, func(context.Context) ([]store.LinearMetadata, error)) error
	LinearBoardMetadata(context.Context) ([]store.LinearMetadata, error)
	ResolveLinearBoardLabel(context.Context, string, string, func(context.Context) (store.LinearMetadata, error)) (string, error)
	ApplyLinearMetadata(context.Context, []store.LinearMetadata) error
}

const boardMetadataTeamsQuery = `query HkBoardMetadataTeams{teams(first:2,filter:{key:{eq:"ROB"}}){nodes{id key updatedAt states(first:100,includeArchived:true){nodes{id name updatedAt archivedAt}pageInfo{hasNextPage}}}pageInfo{hasNextPage}}}`
const boardMetadataLabelsQuery = `query HkBoardMetadataLabels($after:String){issueLabels(first:100,after:$after,includeArchived:true){nodes{id name updatedAt archivedAt team{id}}pageInfo{hasNextPage endCursor}}}`

func (a LinearAPI) fetchBoardMetadata(ctx context.Context) ([]store.LinearMetadata, error) {
	var teams struct {
		Teams struct {
			Nodes []struct {
				ID, Key   string
				UpdatedAt time.Time
				States    struct {
					Nodes []struct {
						ID, Name   string
						UpdatedAt  time.Time
						ArchivedAt *time.Time
					}
					PageInfo struct{ HasNextPage bool }
				}
			}
			PageInfo struct{ HasNextPage bool }
		}
	}
	if err := a.graphql(ctx, linearWriteTimeout, boardMetadataTeamsQuery, nil, &teams); err != nil {
		return nil, err
	}
	if teams.Teams.PageInfo.HasNextPage || len(teams.Teams.Nodes) != 1 {
		return nil, errors.New("ROB metadata team incomplete")
	}
	t := teams.Teams.Nodes[0]
	if t.States.PageInfo.HasNextPage {
		return nil, errors.New("ROB metadata states incomplete")
	}
	items := []store.LinearMetadata{{Kind: "team", ID: t.ID, Name: t.Key, UpdatedAt: t.UpdatedAt}}
	for _, state := range t.States.Nodes {
		if state.ArchivedAt != nil && state.ArchivedAt.After(state.UpdatedAt) {
			state.UpdatedAt = *state.ArchivedAt
		}
		items = append(items, store.LinearMetadata{Kind: "state", ID: state.ID, TeamID: t.ID, Name: state.Name, UpdatedAt: state.UpdatedAt, Deleted: state.ArchivedAt != nil})
	}
	after := ""
	seen := map[string]bool{}
	for {
		var out struct {
			IssueLabels *struct {
				Nodes []struct {
					ID, Name   string
					UpdatedAt  time.Time
					ArchivedAt *time.Time
					Team       *struct{ ID string }
				}
				PageInfo *struct {
					HasNextPage *bool
					EndCursor   string
				}
			}
		}
		if err := a.graphql(ctx, linearWriteTimeout, boardMetadataLabelsQuery, map[string]any{"after": boardMetadataCursor(after)}, &out); err != nil {
			return nil, err
		}
		if out.IssueLabels == nil || out.IssueLabels.Nodes == nil || out.IssueLabels.PageInfo == nil || out.IssueLabels.PageInfo.HasNextPage == nil {
			return nil, errors.New("linear metadata labels incomplete")
		}
		for _, label := range out.IssueLabels.Nodes {
			if label.ID == "" || seen[label.ID] {
				return nil, errors.New("linear metadata duplicate label")
			}
			seen[label.ID] = true
			if label.ArchivedAt != nil && label.ArchivedAt.After(label.UpdatedAt) {
				label.UpdatedAt = *label.ArchivedAt
			}
			m := store.LinearMetadata{Kind: "label", ID: label.ID, Name: label.Name, UpdatedAt: label.UpdatedAt, Deleted: label.ArchivedAt != nil}
			if label.Team != nil {
				m.TeamID = label.Team.ID
			}
			items = append(items, m)
		}
		if !*out.IssueLabels.PageInfo.HasNextPage {
			break
		}
		next := out.IssueLabels.PageInfo.EndCursor
		if next == "" || next == after || len(out.IssueLabels.Nodes) == 0 {
			return nil, errors.New("linear metadata cursor did not advance")
		}
		after = next
	}
	return items, nil
}
func boardMetadataCursor(s string) any {
	if s == "" {
		return nil
	}
	return s
}
func (a LinearAPI) localBoardTarget(ctx context.Context) (BoardTarget, error) {
	if a.Metadata == nil {
		return BoardTarget{}, errors.New("linear metadata store unavailable")
	}
	if err := a.Metadata.EnsureLinearBoardMetadata(ctx, a.fetchBoardMetadata); err != nil {
		return BoardTarget{}, err
	}
	items, err := a.Metadata.LinearBoardMetadata(ctx)
	if err != nil {
		return BoardTarget{}, err
	}
	target := BoardTarget{States: map[string]string{}}
	for _, m := range items {
		if m.Kind == "team" && m.Name == "ROB" && !m.Deleted {
			if target.TeamID != "" {
				return BoardTarget{}, errors.New("ambiguous ROB metadata")
			}
			target.TeamID = m.ID
		}
	}
	if target.TeamID == "" {
		return target, errors.New("ROB metadata team missing")
	}
	for _, m := range items {
		if m.Kind == "state" && m.TeamID == target.TeamID && !m.Deleted {
			if target.States[m.Name] != "" {
				return BoardTarget{}, fmt.Errorf("ambiguous state %q", m.Name)
			}
			target.States[m.Name] = m.ID
		}
	}
	return target, nil
}

func (a LinearAPI) RefreshLinearMetadata(ctx context.Context) error {
	if a.Metadata == nil {
		return errors.New("linear metadata store unavailable")
	}
	if err := a.Metadata.RefreshLinearBoardMetadata(ctx, a.fetchBoardMetadata); err != nil {
		return err
	}
	a.BoardControl.metadataChanged()
	return nil
}
