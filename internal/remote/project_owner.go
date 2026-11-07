package remote

import (
	"context"
	"fmt"
	"net/url"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// ListProjectOwners answers which design session owns each configured
// project, with the operator-desk fallback already resolved server-side.
func (c Client) ListProjectOwners(ctx context.Context) ([]store.ProjectOwner, error) {
	var out struct {
		Projects []store.ProjectOwner `json:"projects"`
	}
	err := c.call(ctx, "GET", "/v1/tasks/projects/owners", nil, &out)
	if err != nil {
		return nil, unsupportedRoute(err, "project_owners_unsupported", "the project owners endpoint")
	}
	return out.Projects, nil
}

// GetProjectOwner reads one project with its recent ownership events.
func (c Client) GetProjectOwner(ctx context.Context, project string) (store.ProjectOwnerDetail, error) {
	var out store.ProjectOwnerDetail
	err := c.call(ctx, "GET", "/v1/tasks/projects/"+url.PathEscape(project)+"/owner", nil, &out)
	if err != nil {
		return out, unsupportedRoute(err, "project_owner_unsupported", "the project owner route")
	}
	return out, nil
}

// SetProjectDesignLane sets or clears (lane "") a project's design session and
// returns the new owner object. The actor is the bearer token's client; the
// client never sends one.
func (c Client) SetProjectDesignLane(ctx context.Context, project, lane, note string) (store.ProjectOwner, error) {
	var out store.ProjectOwner
	err := c.call(ctx, "PUT", "/v1/tasks/projects/"+url.PathEscape(project)+"/owner", map[string]string{"design_lane": lane, "note": note}, &out)
	if err != nil {
		return out, unsupportedRoute(err, "project_owner_unsupported", "the project owner route")
	}
	if out.DesignLane != lane {
		return out, fmt.Errorf("project_owner_not_applied: server response has design_lane=%q, want %q — it likely predates the project owner field", out.DesignLane, lane)
	}
	return out, nil
}
