package youtrack

import (
	"context"
	"net/url"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// Store is the slice of internal/store the search reads. *store.Store
// implements it; tests use an in-memory fake.
type Store interface {
	ListYouTrackProjects(ctx context.Context) ([]store.YouTrackProject, error)
	DocumentKeyExists(ctx context.Context, key string) (bool, error)
	YouTrackTaskHits(ctx context.Context, tenant string, issueIDs []string, lane string) (map[string]store.SearchResult, error)
	YouTrackDocHits(ctx context.Context, articleIDs []string, session string) (map[string]store.SearchResult, error)
}

// Options tune the service. Zero values get the documented defaults.
type Options struct {
	Tenant string
	// SearchTimeout bounds the YouTrack half of one search; zero means
	// DefaultSearchTimeout.
	SearchTimeout time.Duration
}

// Service answers hk search from YouTrack when HANDOFFKEEP_SEARCH_BACKEND is
// "youtrack". It reads the mirror rows and the article ledger through Store.
type Service struct {
	store         Store
	client        *Client
	tenant        string
	searchTimeout time.Duration
}

func NewService(st Store, client *Client, opt Options) *Service {
	if opt.Tenant == "" {
		opt.Tenant = tenantFromClient(client)
	}
	if opt.SearchTimeout <= 0 {
		opt.SearchTimeout = DefaultSearchTimeout
	}
	return &Service{store: st, client: client, tenant: opt.Tenant, searchTimeout: opt.SearchTimeout}
}

func tenantFromClient(c *Client) string {
	if c == nil {
		return ""
	}
	u, err := url.Parse(c.baseURL)
	if err != nil || u.Host == "" {
		return c.baseURL
	}
	return u.Host
}

// Tenant is the host of the configured YouTrack: the tenant hk's bindings
// and mirror links are recorded under.
func (c *Client) Tenant() string { return tenantFromClient(c) }

func (s *Service) Tenant() string { return s.tenant }
