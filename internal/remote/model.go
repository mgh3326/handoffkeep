package remote

import (
	"context"
	"fmt"
	"net/url"

	"github.com/mgh3326/handoffkeep/internal/store"
)

func (c Client) GetProjectMetadata(ctx context.Context, name string) (store.ProjectMetadata, error) {
	var out store.ProjectMetadata
	err := c.call(ctx, "GET", "/v1/tasks/projects/"+url.PathEscape(name)+"/metadata", nil, &out)
	return out, err
}
func (c Client) SetProjectMetadata(ctx context.Context, p store.ProjectMetadata) (store.ProjectMetadata, error) {
	var out store.ProjectMetadata
	err := c.call(ctx, "PUT", "/v1/tasks/projects/"+url.PathEscape(p.Name)+"/metadata", p, &out)
	return out, err
}
func (c Client) SaveMilestone(ctx context.Context, m store.ProjectMilestone) (store.ProjectMilestone, error) {
	var out store.ProjectMilestone
	method, path := "POST", "/v1/milestones"
	if m.ID > 0 {
		method, path = "PUT", fmt.Sprintf("/v1/milestones/%d", m.ID)
	}
	err := c.call(ctx, method, path, m, &out)
	return out, err
}
func (c Client) SetTaskMilestone(ctx context.Context, id int64, milestone *int64) (store.Task, error) {
	var out store.Task
	err := c.call(ctx, "POST", fmt.Sprintf("/v1/tasks/%d/milestone", id), map[string]any{"milestone_id": milestone}, &out)
	return out, err
}

func (c Client) PatchProjectMetadata(ctx context.Context, name string, p store.ProjectMetadataPatch) (store.ProjectMetadata, error) {
	var out store.ProjectMetadata
	err := c.call(ctx, "PATCH", "/v1/tasks/projects/"+url.PathEscape(name)+"/metadata", p, &out)
	return out, err
}

func (c Client) GetMilestone(ctx context.Context, id int64) (store.ProjectMilestone, error) {
	var out store.ProjectMilestone
	err := c.call(ctx, "GET", fmt.Sprintf("/v1/milestones/%d", id), nil, &out)
	return out, err
}

func (c Client) SetTaskProjectWithMetadata(ctx context.Context, id int64, project, note string, patch store.ProjectMetadataPatch) (store.Task, error) {
	var out store.Task
	err := c.call(ctx, "POST", fmt.Sprintf("/v1/tasks/%d/project", id), map[string]any{"project": project, "note": note, "metadata": patch}, &out)
	return out, err
}

func (c Client) PatchMilestone(ctx context.Context, id int64, p store.MilestonePatch) (store.ProjectMilestone, error) {
	var out store.ProjectMilestone
	err := c.call(ctx, "PATCH", fmt.Sprintf("/v1/milestones/%d", id), p, &out)
	return out, err
}
