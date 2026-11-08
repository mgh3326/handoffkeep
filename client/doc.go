// Package client is the public Go client for the handoffkeep (hk) /v1 API: the
// one transport hk's own CLI uses (through internal/remote) and the calls the
// fleet services (panewire, mergewake) make over HTTP.
//
// It never loads a credential. The caller sets Client.URL, Client.Token and,
// for an hk URL behind Cloudflare Access, both halves of the CF Access
// service-token pair. There is no environment reader, no config.env reader and
// no default token: each service passes its own hk client id (svc-panewire,
// svc-mergewake), and none reads the hk CLI's config.env (hk:doc
// decision/2026-11-08/fleet-dependency-boundaries §4).
//
// The package imports only the standard library (and hk's stdlib-only
// internal/buildinfo for the User-Agent revision), never hk's store package,
// so an importer's build carries no hk server dependency (same decision, §1
// and §5).
// The wire types model only the fields the fleet reads. They are additive
// within v0: a field is removed only after every fleet caller stops reading
// it. internal/clientparity keeps them in step with the store types the server
// encodes.
package client
