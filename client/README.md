# client — the public hk API client

`github.com/mgh3326/handoffkeep/client` is the Go client for hk's bearer-token
`/v1` API. It holds the one transport hk uses: the hk CLI reaches it through
`internal/remote`, and the fleet services (panewire, mergewake) import it to
call hk over HTTP instead of running the hk CLI (hk:doc
`decision/2026-11-08/fleet-dependency-boundaries` §1, §5).

It imports only the standard library and hk's stdlib-only
`internal/buildinfo`. It never imports hk's store package, so importing it
adds no hk server dependency (pgx and others) to your build.
`TestClientImportsOnlyStandardLibrary` enforces this.

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

Every request carries `Authorization: Bearer <token>` and an explicit
`User-Agent: handoffkeep/<revision>`. The CF Access headers go only to the
configured URL, and only when both halves are set. A redirect to another host
or scheme is never followed, same-host redirects stop after ten hops, and a
presign answer (`AttachmentURL`) is returned, never followed.

## Calls

`GetTask`, `ClaimTask`, `RebindTaskJob`, `ReopenTask`, `CommentTask`,
`ProjectOwner`, `GetDocument`, `PutDocument`, `PutAttachment`,
`GetAttachment`, `AttachmentURL` and `AskDecision`. Each uses the same route
and JSON as its `internal/remote` equivalent. A non-2xx answer is a
`*client.HTTPError`; branch on `client.ErrorCode(err)`, never on the error
text.

## Stability

The wire types model only the fields the fleet reads. Within v0 they change
additively: a field is removed only after every fleet caller stops reading
it. `internal/clientparity` checks each field against the store type the
server encodes, so a renamed store field breaks the hk build, not a fleet
service at run time. To add a field, copy its JSON tag from the store type
and add the type pair to `internal/clientparity` if it is a new type.
