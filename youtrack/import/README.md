# handoffkeep → YouTrack custom import

`client.js` implements the YouTrack Cloud custom import client. Create a Custom import package in Administration → Integrations → Imports, copy `client.js` into its `client.js` file, and use the import wizard's configured source URL and credentials. The import is a one-time snapshot, not Continuous import. It can be re-run by the source IDs carried in each imported entity.

For a bounded test import, add URL fragment options (fragments are stripped before requests reach hk): `#testProject=HKSPK&sourceProject=handoffkeep&maxTasks=100&maxDocuments=100`. The import wizard sees only the test project `HKSPK`, while the source pages are limited to the selected hk project and row counts. Use `sourceProject=DEC` to exercise decisions in a separate bounded import. Remove the fragment for the complete import. Do not put credentials in URL fragments.

## Source access

Use the existing Cloudflare Access service-token route from YouTrack to hk. In the import wizard set:

- **URL**: the HTTPS hk origin ending at the service path root (no credential in the URL).
- **Username/login**: the Cloudflare Access client ID.
- **Password/token**: a JSON object with `hkToken` (the dedicated hk token) and `accessClientSecret` (the Cloudflare Access service-token secret). Keep the value in the import configuration's protected password field; do not paste it into source files or logs.

The hk auth file contains both `HANDOFFKEEP_TOKEN_youtrack=<token>` and `HANDOFFKEEP_IMPORT_READONLY_youtrack=true`. The import-readonly grant is opt-in and limits that client to `GET /v1/import/*`; other `/v1` reads and all writes receive 403. Continue to enforce Cloudflare Access in front of hk. No credential values belong in this repo.

The export routes are:

- `/v1/import/projects?after=<name>&limit=<n>`
- `/v1/import/tasks?project=<name>&after_id=<id>&limit=<n>` (omit `project` for all task rows; `project=__unassigned__` selects the null-project bucket)
- `/v1/import/comments?after_id=<id>&limit=<n>`
- `/v1/import/documents?project=<name>&after_id=<id>&limit=<n>` (`Shared` means no task link; `Unassigned` means a linked task with no project)
- `/v1/import/decisions?after_id=<id>&limit=<n>`

Every response is `{ "items": [...], "next_cursor": value|null }`. `next_cursor` is exclusive and stable; the import client continues until it is null. Document pages contain at most 20 full-text rows to bound response size. The endpoint never writes or returns attachment bytes.

## What the client imports

Projects are the configured hk task projects, plus `Unassigned`, `Shared`, and `DEC`. Task and decision issues, task comments, and all text documents carry stable hk source IDs, source authors where present, and source timestamps. Large text documents are split into 240 KiB UTF-8-safe parts with an index article. Binary and large artifacts remain in Drive/R2 and are referenced from text.

The script intentionally returns empty issue/article update pages. Do not enable Continuous import until hk has a reliable delta contract and the operator approves a separate change. Agile boards, saved searches and dashboards need YouTrack REST/Hub REST setup after the spike; the custom import API does not support those entities.

The import client uses YouTrack's documented custom-import entry points (`getProjects`, `getProject`, `getIssues`, `getUsers`, `getArticles`) and the YouTrack scripting HTTP connection API. The target project field types and whether authors/timestamps survive are acceptance questions for the spike, not assumed facts.
