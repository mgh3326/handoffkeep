# handoffkeep

`handoffkeep` is a small PostgreSQL-backed context warehouse for agent
checkpoints, memory, and handoff documents. It provides one HTTP core through
a CLI and MCP (stdio and authenticated streamable HTTP).

## 30-second start

```sh
export HANDOFFKEEP_URL=http://127.0.0.1:8800
export HANDOFFKEEP_TOKEN='operator-provided-token'
handoffkeep ctx checkpoint --session demo --kind checkpoint --title 'started' --body 'next: verify deployment'
handoffkeep ctx checkpoint --session demo --title 'PR saved' --body 'review next' --ref prs=26 --ref jobs=hk-v0
handoffkeep ctx recent --session demo
handoffkeep ctx search 'review' --session demo
handoffkeep memory push --agent codex --dir ./memory --apply
```

For persistent local settings use mode 0600
`~/.config/handoffkeep/config.env` with `HANDOFFKEEP_URL=` and
`HANDOFFKEEP_TOKEN=`. `memory push`, `memory pull`, and `doc import` dry-run
unless `--apply` is supplied.

When `HANDOFFKEEP_URL` sits behind Cloudflare Access, a service token lets the
CLI through without a browser login: set `HANDOFFKEEP_CF_ACCESS_CLIENT_ID` and
`HANDOFFKEEP_CF_ACCESS_CLIENT_SECRET` together (env or config.env, same
precedence as the URL/token; whitespace-only values count as unset, and
setting only one is a config error). Every hk request then carries
`CF-Access-Client-Id`/`CF-Access-Client-Secret` plus an explicit
`User-Agent`; a redirect to the Access login fails with
`cf_access_login_redirect` instead of being followed, and the pair is never
sent to a different host or under a different scheme — a redirect that
switches `http`/`https` on the same host is refused as
`redirect_scheme_downgrade`/`redirect_scheme_change`. Redirect Locations that
cannot be parsed surface as `redirect_location_invalid`, never with the
Location text. Only Go's own canonical percent-q rendering of a bad Location is
recognized; noncanonical alternate escaping is not, though Go's rendering can
itself contain hex or unicode escapes for control characters, and those are
accepted. The default or shipped CLI and stdio set no client timeout and always
record the redirect response; a library caller that passes its own
positive-timeout client to `NewStdio` instead falls back to matching that
canonical text. With the pair configured, an attachment download that answers
a `text/html` page without an attachment disposition is
`unexpected_html_response` rather than saved bytes. A `text/html` body on a
JSON API call is rejected the same way with or without the keys, and a
`tasks add` rejected with `invalid_context` reports the server's own error
text instead of the retired `create_project_rejected` message.

## MCP registration

Local stdio delegates to the configured HTTP service:

```json
{"mcpServers":{"handoffkeep":{"command":"handoffkeep","args":["mcp"]}}}
```

For Codex, place the equivalent command entry in its MCP configuration. Remote
streamable MCP is `http://host:8800/mcp` and requires the same Bearer token.
Available tools are `search_context`, `recent_checkpoints`, `put_checkpoint`,
`get_document`, `put_document`, `list_documents`, `memory_get`, `memory_put`,
and `memory_list`.

## Operations

Set `HANDOFFKEEP_DB_URL`, `HANDOFFKEEP_AUTH_FILE`, and optionally
`HANDOFFKEEP_LISTEN_TAILNET`; the auth file contains lines such as
`HANDOFFKEEP_TOKEN_mac-personal=...`. Run the bootstrap SQL once, then install
`deploy/systemd/handoffkeep.service`. Normal serving is loopback-only;
`--listen-tailnet` only accepts a `100.64.0.0/10` address. Wildcard and public
binds are rejected.

The existing `at-pg-backup` service includes the `handoffkeep` database. This
repository intentionally ships no backup service or timer.
