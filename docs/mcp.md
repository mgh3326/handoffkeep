# MCP for ChatGPT (`/mcp`)

hk serves the MCP streamable HTTP transport at `/mcp`. ChatGPT connects to
`https://hk-mcp.robinco.dev/mcp` through Cloudflare Access with Managed OAuth,
so the operator signs in once and the origin receives a signed Access user
assertion. The existing bearer-token clients keep using the same route.

## Dots requests

Create a standalone hk request with `handoffkeep dots request --title TITLE
--body BODY`, optionally adding `--requester-lane L` and `--project P`.
The authenticated caller is the default requester lane. The equivalent API
is `POST /v1/dots/requests` with `title`, `body`, `requester_lane`, and
`project`. Each request is a task in the `dots` lane; its body is an hk
document and its `parent_lane` identifies the requester.

On the authenticated ChatGPT connection, `dots_inbox` lists open requests in
creation order. Use the returned `next_cursor` to fetch another page.
`dots_reply` takes `task_id`, structured `kind`, `body`, and an
`idempotency_key`. Reuse the key only when retrying the same reply. For `START`,
pass the task's `updated_at` from the inbox as `expected_updated_at`. START
claims the task for dots only if that revision is still current and unclaimed.
`BLOCKED` parks it with the reason, and `RESULT`
completes it with the result document as evidence. A `QUESTION` is delivered
to the requester; set `ask_operator` when it needs an hk decision request and
the task should await that decision. RESULT, BLOCKED, and QUESTION also create
a result document, result record, and lane notification. START creates only a
comment and the claim. Replies are attributed to the authenticated operator.

The result's `source` is `hk` or `linear`. Existing Linear requests and
webhooks remain independent and do not need a channel setting.

`HANDOFFKEEP_DOTS_REPLY_WRITES` controls replies. It defaults to `off`, which
leaves `dots_inbox` available and makes `dots_reply` return
`dots_reply_writes_disabled`. Set it to `on` to enable replies, or to `off`
to stop them again.

## Proxied hosts

cloudflared reaches the origin over loopback (`127.0.0.1:8800`) and forwards
the public `Host`, for example `hk-mcp.robinco.dev`. The MCP SDK's DNS
rebinding guard rejects a loopback request whose `Host` is not loopback, which
is meant for unauthenticated localhost servers; `/mcp` authenticates every
request before the SDK transport sees it, so `HTTPHandler` disables that guard
and a proxied request is decided by its credential alone. The temporary
cloudflared `httpHostHeader: localhost` workaround on the `hk-mcp` route is
therefore unnecessary once this build is deployed and can be removed.

## Credentials

A request authenticates with either credential:

- **hk bearer token**, unchanged: `Authorization: Bearer <token>`. The client
  id names the principal as before.
- **Cloudflare Access email assertion**: `Cf-Access-Jwt-Assertion` for the MCP
  application's audience. The origin verifies the signature against the team
  JWKS, requires RS256, issuer, audience, expiration and an allowlisted email.

Access **service-token** identities are not accepted on this path: they have no
email, and `/mcp` is an operator surface. The existing service-token path on
the `hk-api` hostname is untouched.

Any failure answers `401` with the body `{"error":"unauthorized"}` and
`WWW-Authenticate: Bearer`. hk does not serve OAuth protected-resource
metadata; Cloudflare's edge serves the OAuth discovery document for this
hostname.

## Configuration

The Access credential is mounted only when all three settings are nonempty
after trimming whitespace. Incomplete configuration disables that credential
and leaves the bearer path working, the same fail-closed rule `/ui` follows.
Startup logs the state without the configured values:

```text
mcp.access enabled
mcp.access disabled reason=incomplete_config team_domain_set=false aud_set=true allowlist_set=false
```

| Environment variable | Purpose |
| --- | --- |
| `HANDOFFKEEP_MCP_CF_TEAM_DOMAIN` | Cloudflare Access team domain, for example `example.cloudflareaccess.com` |
| `HANDOFFKEEP_MCP_CF_AUD` | Access application audience tag for the MCP hostname |
| `HANDOFFKEEP_MCP_ALLOWED_EMAILS` | Comma-separated email allowlist admitted on `/mcp` |

The JWKS endpoint is derived as
`https://<team-domain>/cdn-cgi/access/certs`. RSA keys are cached for ten
minutes, and an unknown key ID triggers at most one refresh per minute, using
the same verifier as `/ui`.

## Cloudflare setup

The design session creates this after the change lands. hk only reads the
assertion.

1. Create a Cloudflare Access application for `hk-mcp.robinco.dev` and point
   the tunnel route at the existing hk server (`/mcp`). Leave `hk-api` and its
   service-token policy alone.
2. Add an Allow policy for the operator's email only.
3. Turn on **Managed OAuth** for the application. Access acts as the OAuth
   authorization server, including PKCE S256 and dynamic client registration /
   client-id metadata document handling, and issues the user JWT that reaches
   the origin.
4. Allow these redirect URIs:
   - `https://chatgpt.com/connector_platform_oauth_redirect`
   - `https://chatgpt.com/connector/oauth/*`
5. Set the three `HANDOFFKEEP_MCP_*` variables on the server and restart.

No Worker OAuth proxy is needed, and hk serves no authorization-server or
protected-resource metadata itself.

## Identity and attribution

An Access identity becomes the principal `operator:<email>` with channel
`chatgpt`, so works written from ChatGPT are attributed like console writes:

- `task_comment` stores `operator:<email>` as the comment author.
- `decision_answer` closes the request on the verified operator path with
  actor `operator:<email>` and provenance `operator_direct`.
- `task_intake` records `source=chatgpt` and a store-name-safe form of the
  principal as `created_by` (the store's intake path accepts names only); the
  tool response reports the exact `operator:<email>` principal and the
  `chatgpt` channel.

The store's decision channel column has no `chatgpt` value and this change
adds no migration, so a verified Access answer is recorded on the existing
`console` channel and a bearer answer on the existing `cli` channel. Human-only
requests therefore stay open for a bearer client and close only through an
Access identity, matching the console and CLI rules.

## Tools

Read tools set `readOnlyHint: true`. Write tools set `readOnlyHint: false` and
`destructiveHint: false`: they add records and never change task state,
priority or lane.

| Tool | Kind | Arguments | Result |
| --- | --- | --- | --- |
| `task_search` | read | `query`, optional `state`/`lane`/`project`, `limit` ≤ 50 | Task summaries from the console search path, including Korean translations |
| `task_show` | read | `id` | Title (with `title_ko` when present), state, priority, lane, project, `refs.pr`, last 20 comments, last 10 events |
| `task_list` | read | optional `state`/`lane`/`project`, `limit` ≤ 100 | Task summaries |
| `decisions_pending` | read | none | Open structured requests with question, options, `question_ko`/`options_ko` when fresh, and state |
| `decision_answer` | write | `request_id`, `option` or `text`, optional `note` | Store result; an already answered or stale request returns its error code |
| `task_comment` | write | `id`, `body` | The appended comment and its author |
| `task_intake` | write | `title`, `body`, `project`, optional `lane` | The triage task, `created`, and duplicate candidates found before filing |

Every task result carries `console_url`
(`https://console.robinco.dev/ui/tasks/<id>`) and, when the task has a YouTrack
mirror, the readable YouTrack id and URL. Limits are clamped, not rejected:
`task_search` stops at 50 and `task_list` at 100.

`task_intake` files through the existing triage intake path. Source is
`chatgpt`; the source reference is a digest of the title, body and project, so
an identical repeat returns `created:false` and the task already recorded.
Candidate duplicates come from the same task search the console uses and are
returned with the new task.

## Turning it off

Unset the three `HANDOFFKEEP_MCP_*` variables and restart: the Access
credential is off and bearer clients keep working. Disabling the Access
application or removing the tunnel route makes `hk-mcp.robinco.dev`
unreachable; `/mcp` on the existing origin is unaffected. The change adds no
schema migration and writes no configuration.
