package mcp

import (
	"context"
	"net/http"
	"strings"

	"github.com/mgh3326/handoffkeep/internal/api"
	"github.com/mgh3326/handoffkeep/internal/cfaccess"
	"github.com/mgh3326/handoffkeep/internal/store"
)

// Request channels name how an MCP write reached the server. They are the
// attribution the tools report; the store's decision channel is separate and
// is chosen by principal.decisionChannel.
const (
	ChannelChatGPT = "chatgpt"
	ChannelBearer  = "bearer"
)

// principal is the authenticated identity of one MCP request. Actor is the
// value every write records; Access identities use the console's operator
// form so the same person reads the same in both surfaces.
type principal struct {
	Actor   string
	Channel string
	Access  bool
}

func (p principal) decisionChannel() string {
	if p.Access {
		return store.DecisionChannelConsole
	}
	return store.DecisionChannelCLI
}

type principalKey struct{}

func withPrincipal(ctx context.Context, p principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

func principalFrom(ctx context.Context) (principal, bool) {
	p, ok := ctx.Value(principalKey{}).(principal)
	return p, ok
}

// authenticate accepts an hk bearer token first, keeping that credential
// exactly as it was, then a Cloudflare Access email assertion when the MCP
// Access verifier is configured. Service-token identities are not a
// credential for this path: they have no email and are refused.
func authenticate(r *http.Request, tokens api.Tokens, access *cfaccess.Verifier) (principal, bool) {
	if client, ok := tokens.Client(r); ok {
		return principal{Actor: client, Channel: ChannelBearer}, true
	}
	if access == nil {
		return principal{}, false
	}
	identity, ok := access.AuthenticatedIdentity(r)
	if !ok || identity.ServiceName != "" {
		return principal{}, false
	}
	email := strings.TrimSpace(identity.Email)
	if email == "" {
		return principal{}, false
	}
	return principal{Actor: "operator:" + email, Channel: ChannelChatGPT, Access: true}, true
}
