package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// ProjectOwner is which design session owns a project.
type ProjectOwner struct {
	Name       string `json:"name"`
	DesignLane string `json:"design_lane"`
	// EffectiveOwner is DesignLane, or the operator-desk fallback the server
	// resolves when no design lane is set.
	EffectiveOwner string     `json:"effective_owner"`
	UpdatedAt      *time.Time `json:"updated_at,omitempty"`
}

// ProjectOwner reads a project's owner. A server that predates the route
// answers project_owner_unsupported.
func (c Client) ProjectOwner(ctx context.Context, project string) (ProjectOwner, error) {
	var out ProjectOwner
	err := c.transport().Call(ctx, "GET", "/v1/tasks/projects/"+url.PathEscape(project)+"/owner", nil, &out)
	var he *HTTPError
	if errors.As(err, &he) && he.Code == "" && (he.Status == http.StatusNotFound || he.Status == http.StatusMethodNotAllowed) {
		return out, fmt.Errorf("project_owner_unsupported: server does not provide the project owner route")
	}
	return out, err
}
