package remote

import (
	"context"
	"net/url"

	"github.com/mgh3326/handoffkeep/internal/store"
)

func (c Client) ListDeployServices(ctx context.Context) ([]store.DeployService, error) {
	var out struct {
		Services []store.DeployService `json:"services"`
	}
	err := c.call(ctx, "GET", "/v1/deploy-services", nil, &out)
	return out.Services, err
}
func (c Client) PutDeployService(ctx context.Context, x store.DeployService) (store.DeployService, error) {
	var out store.DeployService
	err := c.call(ctx, "PUT", "/v1/deploy-services/"+url.PathEscape(x.Name), map[string]any{"repo": x.Repo, "record_prefixes": x.RecordPrefixes}, &out)
	return out, err
}
func (c Client) DeactivateDeployService(ctx context.Context, name string) (store.DeployService, error) {
	var out store.DeployService
	err := c.call(ctx, "POST", "/v1/deploy-services/"+url.PathEscape(name)+"/deactivate", nil, &out)
	return out, err
}
