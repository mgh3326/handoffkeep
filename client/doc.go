// Package client is the public Go client for the handoffkeep (hk) /v1 API:
// the calls the fleet services (panewire, mergewake) make over HTTP, and their
// wire types.
//
// It offers fleet calls only. It exports no way to build or send a raw hk
// request — no request builder, no sender, no raw *http.Client and no call
// that takes a path — because every request carries the bearer token and the
// Cloudflare Access pair. Raw requests stay inside hk, in its module-internal
// transport (internal/hktransport), which holds each request to the scheme
// and host of Client.URL and never puts a redirect's Location in an error
// (hk 3544). A missing call is added here as a fleet call;
// TestClientExportsOnlyFleetCalls keeps the exported names to that list.
//
// It never loads a credential. The caller sets Client.URL, Client.Token and,
// for an hk URL behind Cloudflare Access, both halves of the CF Access
// service-token pair. There is no environment reader, no config.env reader and
// no default token: each service passes its own hk client id (svc-panewire,
// svc-mergewake), and none reads the hk CLI's config.env (hk:doc
// decision/2026-11-08/fleet-dependency-boundaries §4).
//
// The package imports only the standard library and two stdlib-only hk
// packages (internal/hktransport and internal/buildinfo), never hk's store
// package, so an importer's build carries no hk server dependency (same
// decision, §1 and §5).
//
// The wire types model only the fields the fleet reads. They are additive
// within v0: a field is removed only after every fleet caller stops reading
// it. internal/clientparity keeps them in step with the store types the server
// encodes.
package client
