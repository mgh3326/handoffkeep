# Fleet console (`/ui`)

The optional fleet console is a read-only view of relay events, the task queue,
decision inbox, session checkpoints, and the configured hub. `GET /ui` redirects
to `GET /ui/timeline`.

## Configuration

The console is mounted only when all three Cloudflare Access settings are
nonempty after trimming whitespace. If any is absent, `/ui` is not registered
and returns the normal 404 response.

| Environment variable | Purpose | Example |
| --- | --- | --- |
| `HANDOFFKEEP_UI_CF_TEAM_DOMAIN` | Cloudflare Access team domain | `example.cloudflareaccess.com` |
| `HANDOFFKEEP_UI_CF_AUD` | Access application audience tag | `example-ui-audience` |
| `HANDOFFKEEP_UI_ALLOWED_EMAILS` | Comma-separated UI email allowlist | `admin@example.com` |
| `HANDOFFKEEP_HUB_URL` | Optional hub origin for fleet status | `http://127.0.0.1:9000` |
| `HANDOFFKEEP_HUB_TOKEN` | Optional server-side hub credential | set outside source control |

Create a Cloudflare Access application for the UI origin, set its team domain
and audience tag in the first two settings, and add an Access policy that
admits only the identities intended to operate the console. The origin repeats
that check: it verifies the assertion signature against the team JWKS, requires
RS256, issuer, audience, expiration, and an allowlisted email. The email check
trims whitespace and compares ASCII case-insensitively.

The JWKS endpoint is derived as
`https://<team-domain>/cdn-cgi/access/certs`. RSA keys are cached for ten
minutes. An unknown key ID triggers one immediate refresh at most once per
minute; no usable cached key or failed retrieval is an unauthenticated request.

## Security boundary

`/ui` has no write routes and accepts only `GET`. It does not read API bearer
tokens. Conversely, `/v1/*` and `/metrics` do not read Cloudflare Access
assertions. This prevents either credential type from substituting for the
other.

The absent-route 404 is fail-closed: an incomplete Access configuration cannot
accidentally expose a console without origin-side verification. Hub credentials
remain in the server process; browser responses, templates, static assets, and
SSE messages contain only the fetched operational fields, never the hub token.

All database content is passed to Go's `html/template` without typed HTML,
JavaScript, or URL wrappers. A task PR is linked only when its stored value
starts with `https://github.com/`; other values are rendered as text.

## Pages and data

- `/ui/timeline` shows relay events in descending durable-ID order. It supports
  `lane`, `kind`, `since`, `until`, and exclusive `before_id` parameters. Date
  bounds apply to `received_at` in UTC (`since` inclusive, `until` inclusive
  through the end of that UTC day). Pages contain at most 200 events; the store
  hard-caps any timeline query at 1000.
- `/ui/queue` is a lane-by-state task board. Expanding a card fetches its
  append-only task transition history.
- `/ui/decisions` shows only unresolved items. The definitions are exact:
  1. A task is shown when `state='needs_decision'`; its question is the `note`
     from that task's latest `task_events` row with `to='needs_decision'`.
  2. A `job.escalate` relay event is shown when no later-ID event with the same
     `job_id` has kind `job.joined` or `job.completed`.
  3. A `lane.event` whose `text` begins `[decision-needed]` is shown when no
     later-ID `lane.event` from the same `owner_lane` has text beginning
     `[decision-answered]`.

  P1 does not create `[decision-answered]` events, so lane decisions normally
  remain open until a later write path is introduced.
- `/ui/fleet` fetches `/v1/nodes` and, when available, `/v1/jobs` through the
  server-side hub proxy. Both calls have a three-second bound. Missing hub
  configuration affects only this panel. A failed nodes fetch retains the last
  successful snapshot and marks it stale; without one the page says the hub is
  unavailable. A 404 or 405 from `/v1/jobs` means the jobs API is unsupported.

## Live refresh and vendored assets

`GET /ui/events` is an SSE stream with `Content-Type: text/event-stream` and
`Cache-Control: no-cache`. It sends an initial `delta` event then polls the
maximum `relay_events.id` and `task_events.id` every ten seconds. Changed IDs
produce:

```text
event: delta
data: {"relay_max_id":123,"task_event_max_id":456}
```

The page uses that event to have htmx fetch its current `/ui/fragments/...`
view. It uses vendored htmx 2.0.4 from `internal/ui/static/htmx.min.js`; the
adjacent `htmx.LICENSE` is the htmx 0BSD license. No external CDN is used.
