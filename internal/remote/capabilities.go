package remote

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
)

// serverCaps is one origin's answer to GET /v1/capabilities.
type serverCaps struct {
	// legacy means the server gave no list: it predates the endpoint, or the
	// read was refused. Its requests go out as they always did.
	legacy   bool
	features map[string]bool
}

// capsCache holds the answers read in this process, keyed by origin. A 404
// (legacy) and a listed answer are kept; other failures are read again next
// time.
var capsCache = struct {
	sync.Mutex
	byOrigin map[string]serverCaps
}{byOrigin: map[string]serverCaps{}}

// capabilitiesOf returns the origin's capabilities, read from the server once
// per process.
func (c Client) capabilitiesOf(ctx context.Context) (serverCaps, error) {
	capsCache.Lock()
	got, ok := capsCache.byOrigin[c.URL]
	capsCache.Unlock()
	if ok {
		return got, nil
	}
	var out struct {
		ProtocolVersion int      `json:"protocol_version"`
		Features        []string `json:"features"`
	}
	err := c.call(ctx, http.MethodGet, "/v1/capabilities", nil, &out)
	var he *HTTPError
	switch {
	case err == nil:
		got = serverCaps{features: make(map[string]bool, len(out.Features))}
		for _, f := range out.Features {
			got.features[f] = true
		}
	case errors.As(err, &he) && he.Status == http.StatusNotFound:
		got = serverCaps{legacy: true}
	case errors.As(err, &he):
		// The server answered but did not give a list: send as before, and
		// ask again on the next call.
		return serverCaps{legacy: true}, nil
	default:
		return serverCaps{}, fmt.Errorf("capabilities_unavailable: %w", err)
	}
	capsCache.Lock()
	capsCache.byOrigin[c.URL] = got
	capsCache.Unlock()
	return got, nil
}

// requireFeature stops a request before anything is sent when the server's
// list omits feature. A legacy server has no list to check, so its requests go
// out as they always did, and its invalid_context answers keep the existing
// translation into a *_rejected error.
func (c Client) requireFeature(ctx context.Context, feature string) error {
	caps, err := c.capabilitiesOf(ctx)
	if err != nil {
		return err
	}
	if caps.legacy || caps.features[feature] {
		return nil
	}
	return fmt.Errorf("server_lacks_feature:%s: the server's /v1/capabilities does not list it, so nothing was sent; the server predates it", feature)
}

// requireFeatureWhen checks feature only when the request carries the field
// it names.
func (c Client) requireFeatureWhen(ctx context.Context, carried bool, feature string) error {
	if !carried {
		return nil
	}
	return c.requireFeature(ctx, feature)
}
