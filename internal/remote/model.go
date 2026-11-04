package remote

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

func (c Client) SetTaskDue(ctx context.Context, id int64, due *time.Time) (store.Task, error) {
	var out store.Task
	err := c.call(ctx, "POST", fmt.Sprintf("/v1/tasks/%d/due", id), map[string]any{"due_at": due}, &out)
	return out, err
}

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
func (c Client) ListLabels(ctx context.Context) ([]store.Label, error) {
	var out struct {
		Labels []store.Label `json:"labels"`
	}
	err := c.call(ctx, "GET", "/v1/labels", nil, &out)
	return out.Labels, err
}
func (c Client) SaveLabel(ctx context.Context, l store.Label) (store.Label, error) {
	var out store.Label
	method, path := "POST", "/v1/labels"
	if l.ID > 0 {
		method, path = "PUT", fmt.Sprintf("/v1/labels/%d", l.ID)
	}
	err := c.call(ctx, method, path, l, &out)
	return out, err
}
func (c Client) RetireLabel(ctx context.Context, id int64) error {
	var out map[string]bool
	return c.call(ctx, "POST", fmt.Sprintf("/v1/labels/%d/retire", id), nil, &out)
}
func (c Client) SetTaskMilestone(ctx context.Context, id int64, milestone *int64) (store.Task, error) {
	var out store.Task
	err := c.call(ctx, "POST", fmt.Sprintf("/v1/tasks/%d/milestone", id), map[string]any{"milestone_id": milestone}, &out)
	return out, err
}
func (c Client) SetTaskLabel(ctx context.Context, id, labelID int64, attach bool) (store.Task, error) {
	var out store.Task
	err := c.call(ctx, "POST", fmt.Sprintf("/v1/tasks/%d/labels", id), map[string]any{"label_id": labelID, "attach": attach}, &out)
	return out, err
}
func (c Client) SetTaskDeadlines(ctx context.Context, id int64, d store.TaskDeadlines) (store.Task, error) {
	var out store.Task
	err := c.call(ctx, "POST", fmt.Sprintf("/v1/tasks/%d/deadlines", id), d, &out)
	return out, err
}
func (c Client) SetTaskSubscriber(ctx context.Context, id int64, subscriber, source string, active bool) (store.Task, error) {
	var out store.Task
	err := c.call(ctx, "POST", fmt.Sprintf("/v1/tasks/%d/subscribers", id), map[string]any{"subscriber": subscriber, "source": source, "active": active}, &out)
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
