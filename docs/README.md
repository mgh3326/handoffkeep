# handoffkeep

`handoffkeep` is a small PostgreSQL-backed context warehouse for agent
checkpoints, memory, and handoff documents. It provides one HTTP core through
a CLI and MCP (stdio and authenticated streamable HTTP).

## 30-second start

```sh
export HANDOFFKEEP_URL=http://127.0.0.1:8800
export HANDOFFKEEP_TOKEN='operator-provided-token'
handoffkeep ctx checkpoint --session demo --kind checkpoint --title 'started' --body 'next: verify deployment'
handoffkeep ctx recent --session demo
handoffkeep memory push --agent codex --dir ./memory --apply
```

For persistent local settings use mode 0600
`~/.config/handoffkeep/config.env` with `HANDOFFKEEP_URL=` and
`HANDOFFKEEP_TOKEN=`. `memory push`, `memory pull`, and `doc import` dry-run
unless `--apply` is supplied.

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

Set `HANDOFFKEEP_DB_URL` and `HANDOFFKEEP_AUTH_FILE`; the latter contains lines
such as `HANDOFFKEEP_TOKEN_mac-personal=...`. Run the bootstrap SQL once, then
install `deploy/systemd/handoffkeep.service`. Normal serving is loopback-only;
`--listen-tailnet` only accepts a `100.64.0.0/10` address. Wildcard and public
binds are rejected.

The existing `at-pg-backup` service includes the `handoffkeep` database. This
repository intentionally ships no backup service or timer.
