# client — the public hk API client

`github.com/mgh3326/handoffkeep/client` is the Go client for hk's bearer-token
`/v1` API. The fleet services (panewire, mergewake) import it to call hk over
HTTP instead of running the hk CLI (hk:doc
`decision/2026-11-08/fleet-dependency-boundaries` §1, §5).

It offers fleet calls only. It exports no way to build or send a raw hk
request: no request builder, no sender, no raw `*http.Client` and no call that
takes a path. Every hk request carries the bearer token and the Cloudflare
Access pair, so raw requests stay inside hk, in the module-internal transport
`internal/hktransport`, which Go does not let another module import. That
transport is the one hk's own CLI uses through `internal/remote`. It sends a
request only to the scheme and host of `Client.URL`, and it never puts a
redirect's `Location` in an error (hk 3544). If you need a call that is not
here, add it to this package as a fleet call. `TestClientExportsOnlyFleetCalls`
fails when an exported name is not on its list.

It imports only the standard library and two stdlib-only hk packages,
`internal/hktransport` and `internal/buildinfo`. It never imports hk's store
package, so importing it adds no hk server dependency (pgx and others) to your
build. `TestClientImportsOnlyStandardLibrary` enforces this.

## Credentials

The package loads no credential: there is no environment reader, no
`config.env` reader and no default token. Each service passes its own hk
client id's token and stops reading the hk CLI's `config.env` (decision §4):

```go
c := client.Client{
	URL:   cfg.HKURL,          // e.g. https://hk.example
	Token: cfg.HKToken,        // the svc-panewire or svc-mergewake token
	// Only when the hk URL is behind Cloudflare Access; set both or neither.
	CFAccessClientID:     cfg.CFAccessID,
	CFAccessClientSecret: cfg.CFAccessSecret,
}
task, found, err := c.GetTask(ctx, 3544)
```

`URL` must be an absolute `http` or `https` URL with a host; a path prefix on
it is kept. Every request carries `Authorization: Bearer <token>` and an
explicit `User-Agent: handoffkeep/<revision>`. The CF Access headers go only to
the configured URL, and only when both halves are set. A redirect to another host
or scheme is never followed, same-host redirects stop after ten hops, and a
presign answer (`AttachmentURL`) is returned, never followed.

## Calls

`GetTask`, `ListTasks`, `ClaimTask`, `RebindTaskJob`, `ReopenTask`,
`CommentTask`, `ProjectOwner`, `GetDocument`, `ListDocuments`,
`PutDocument`, `PutAttachment`, `GetAttachment`, `AttachmentURL`,
`RecordDecisionRequest`, `AskDecision` and `ListQuotaObservations`. Each uses
its documented `/v1` route and JSON shape; `internal/clientparity` checks that
the two list calls send the same query. `ListTasks` and `ListDocuments` take a
limit from 1 to 1000 and return at most that many rows, with no sign that
more exist. `ListDocuments` matches its prefix with SQL `LIKE` (`_` and `%`
are wildcards) and returns documents without their body.
`RecordDecisionRequest` only records the request (the CLI's `decisions ask`);
`AskDecision` also opens its Linear issue (`decisions ask --linear`) and fails
with `503 linear_decisions_disabled` when hk runs without Linear decisions. A
service that must not open issues uses `RecordDecisionRequest`. Both take the
same `DecisionAskInput`. A non-2xx answer is a
`*client.HTTPError`; branch on `client.ErrorCode(err)`, never on the error
text.

## Stability

The wire types model only the fields the fleet reads. Within v0 they change
additively: a field is removed only after every fleet caller stops reading
it. `internal/clientparity` checks each field against the store type the
server encodes, so a renamed store field breaks the hk build, not a fleet
service at run time. To add a field, copy its JSON tag from the store type
and add the type pair to `internal/clientparity` if it is a new type.
