package remote

import (
	"context"
	"fmt"
	"net/url"
	"strconv"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// PutService registers or replaces a service (operator clients only).
func (c Client) PutService(ctx context.Context, x store.Service) (store.Service, error) {
	in := map[string]any{"repo": x.Repo, "path_patterns": x.PathPatterns, "deploy_mode": x.DeployMode, "targets": x.Targets}
	var out store.Service
	err := c.call(ctx, "PUT", "/v1/services/"+url.PathEscape(x.Name), in, &out)
	return out, err
}

// ListServices reads the service registry.
func (c Client) ListServices(ctx context.Context) ([]store.Service, error) {
	var out struct {
		Services []store.Service `json:"services"`
	}
	err := c.call(ctx, "GET", "/v1/services", nil, &out)
	return out.Services, err
}

// PendingChanges reads the changes a service has not shipped to every target.
func (c Client) PendingChanges(ctx context.Context, service string, limit int) ([]store.Change, error) {
	var out struct {
		Changes []store.Change `json:"changes"`
	}
	err := c.call(ctx, "GET", "/v1/services/"+url.PathEscape(service)+"/pending?limit="+strconv.Itoa(limit), nil, &out)
	return out.Changes, err
}

// StartDeploy records an attempt. created is false when the same attempt was
// already recorded with the same payload.
func (c Client) StartDeploy(ctx context.Context, in store.DeployStart) (store.Deploy, bool, error) {
	var out struct {
		Created *bool        `json:"created"`
		Deploy  store.Deploy `json:"deploy"`
	}
	if err := c.call(ctx, "POST", "/v1/deploys", in, &out); err != nil {
		return store.Deploy{}, false, err
	}
	if out.Created == nil || out.Deploy.ID < 1 {
		return store.Deploy{}, false, fmt.Errorf("deploy_response_malformed: server response has no created flag or deploy id")
	}
	return out.Deploy, *out.Created, nil
}

// FinishDeploy ends an attempt.
func (c Client) FinishDeploy(ctx context.Context, attemptID string, in store.DeployFinish) (store.Deploy, error) {
	var out store.Deploy
	err := c.call(ctx, "POST", "/v1/deploys/"+url.PathEscape(attemptID)+"/finish", in, &out)
	return out, err
}
