# O3 contract fixture pack (hk 1924, M1)

Machine-readable contract for the O3 authority design, generated from a
single pinned design document:

- key: `designs/hk1873/b1873a-sol-r4`
- document: 44185, revision 4, review amendment 1
- body sha256: `ce4f6efbe39514bb0bced2d0a66510e646058a66d0d28bbb417af180f375c19d`

The pack changes only when a new design revision exists; the manifest
pins the design body sha and the sha256 of every pack file, so any
fixture edit without a matching manifest update fails verification.
Manifest hash changes without a new design revision must be rejected
in review.

## Files

- `contract.json` — the section 5 JSON Schema 2020-12 bundle, copied
  byte-exact from the design's fenced json block. Closed Request union,
  per-kind Input/Payload definitions, Error enumeration, endpoints and
  Policy schema.
- `vectors/*.json` — every normative vector table converted to data,
  one file per table. Each case is `{name, source, given, request,
  expect}`; `source` names the design section, the design's own table
  name, the table row number and the verbatim row text. `given` holds
  prior journal records and policy as Record objects; `request` is a
  Request object for HTTP cases, `{type:"cli"}` argv/stdin for section
  6b cases, `{type:"legacy"}` for fenced legacy routes, or
  `{type:"policy"|"meta"|"none"}` for non-HTTP expectations.
- `legacy/` — sanitized legacy event fixtures for the import vectors:
  synthetic job dirs with `events/0000N-<kind>.json` in today's format.
  Every id, hash and path is synthetic; no real job dir, inbox, event
  file, hostname or home path was copied.
- `matrix.json` — the section 6 command-by-mode matrix as data.
- `cli.json` — the section 6b subcommand table and stable-code to
  process-exit map as data.
- `permmap.json` — the section 5 permission-to-operation map.
- `inventory.json` — the section 6a receipt-consumer inventory.
- `rollout.json` — the section 7 rollout phases.
- `MANIFEST.json` — the design key, revision and body sha256, plus the
  sha256 and size of every file in the pack.

## Consumers

- S1 (the handoffkeep server, hk 1925) consumes this pack by an in-repo
  Go test: `go test -p 2 ./internal/o3contract/...` verifies the bundle,
  validates every vector's request and expected record against the
  contract schemas, and pins per-table case counts.
- C1 (the wrk client, hk 1926) consumes it later by vendoring the pack
  into agent-skills at a pinned MANIFEST sha.

The pack contains no secret, token value or environment value.
