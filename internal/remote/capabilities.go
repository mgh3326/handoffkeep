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
	// legacy means the server answered 404: it predates the endpoint, so it
	// lists nothing and its requests go out as they always did.
	legacy   bool
	features map[string]bool
}

// capsCache holds the answers read in this process, keyed by origin. Only a
// listed answer and a 404 are kept; any other failure is read again next time.
var capsCache = struct {
	sync.Mutex
	byOrigin map[string]serverCaps
	flight   map[string]*capabilitiesFlight
}{byOrigin: map[string]serverCaps{}}

type capabilitiesFlight struct {
	done chan struct{}
	caps serverCaps
	err  error
}

// capabilitiesOf returns the origin's capabilities, read from the server once
// per process.
func (c Client) capabilitiesOf(ctx context.Context) (serverCaps, error) {
	capsCache.Lock()
	got, ok := capsCache.byOrigin[c.URL]
	if ok {
		capsCache.Unlock()
		return got, nil
	}
	if flight := capsCache.flight[c.URL]; flight != nil {
		capsCache.Unlock()
		select {
		case <-flight.done:
			return flight.caps, flight.err
		case <-ctx.Done():
			return serverCaps{}, ctx.Err()
		}
	}
	flight := &capabilitiesFlight{done: make(chan struct{})}
	if capsCache.flight == nil {
		capsCache.flight = map[string]*capabilitiesFlight{}
	}
	capsCache.flight[c.URL] = flight
	capsCache.Unlock()

	got, err := c.fetchCapabilities(ctx)
	capsCache.Lock()
	flight.caps, flight.err = got, err
	if err == nil {
		capsCache.byOrigin[c.URL] = got
	}
	delete(capsCache.flight, c.URL)
	close(flight.done)
	capsCache.Unlock()
	return got, err
}

// fetchCapabilities reads GET /v1/capabilities. Only a 404 establishes that
// the endpoint is absent. Any other failure, including a refusal or a
// transient 5xx, means the server's shapes are unknown, so the caller must not
// send a gated shape.
func (c Client) fetchCapabilities(ctx context.Context) (serverCaps, error) {
	var out struct {
		ProtocolVersion int      `json:"protocol_version"`
		Features        []string `json:"features"`
	}
	err := c.call(ctx, http.MethodGet, "/v1/capabilities", nil, &out)
	var he *HTTPError
	switch {
	case err == nil:
		got := serverCaps{features: make(map[string]bool, len(out.Features))}
		for _, f := range out.Features {
			got.features[f] = true
		}
		return got, nil
	case errors.As(err, &he) && he.Status == http.StatusNotFound:
		return serverCaps{legacy: true}, nil
	default:
		return serverCaps{}, fmt.Errorf("capabilities_unavailable: %w", err)
	}
}

// requireFeature stops a request before anything is sent when the server's
// list omits feature. A legacy (404) server has no list to check, so its
// requests go out as they always did, and its invalid_context answers keep the
// existing translation into a *_rejected error.
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
