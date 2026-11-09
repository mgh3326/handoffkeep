# handoffkeep → YouTrack custom import

`client.js` implements the YouTrack Cloud custom import client. Create a Custom import package in Administration → Integrations → Imports, copy `client.js` into its `client.js` file, and use the import wizard's configured source URL and credentials. The initial import and continuous updates use immutable hk source IDs.

For a bounded test import, add URL fragment options (fragments are stripped before requests reach hk): `#testProject=HKSPK&sourceProject=handoffkeep&maxTasks=100&maxDocuments=100`. The import wizard sees only the test project `HKSPK`, while the source pages are limited to the selected hk project and row counts. Use `sourceProject=DEC` to exercise decisions in a separate bounded import. Remove the fragment for the complete import. Do not put credentials in URL fragments.

## Source access

Use the existing Cloudflare Access service-token route from YouTrack to hk. In the import wizard set:

- **URL**: the HTTPS hk origin ending at the service path root (no credential in the URL).
- **Username/login**: the Cloudflare Access client ID.
- **Password/token**: a JSON object with `hkToken` (the dedicated hk token) and `accessClientSecret` (the Cloudflare Access service-token secret). Keep the value in the import configuration's protected password field; do not paste it into source files or logs.

The hk auth file contains both `HANDOFFKEEP_TOKEN_youtrack=<token>` and `HANDOFFKEEP_IMPORT_READONLY_youtrack=true`. The import-readonly grant is opt-in and limits that client to `GET /v1/import/*`; other `/v1` reads, `/mcp`, and all writes receive 403. Continue to enforce Cloudflare Access in front of hk. No credential values belong in this repo. Before rolling back to a binary that predates import grants, remove the `HANDOFFKEEP_IMPORT_READONLY_*` line from the auth file; that older binary rejects unknown auth-file keys.

The export routes are:

- `/v1/import/projects?after=<name>&limit=<n>`
- `/v1/import/tasks?project=<name>&after_id=<id>&limit=<n>` (omit `project` for all task rows; `project=/unassigned` selects the null-project bucket; `project=/shared` returns an empty task page)
- `/v1/import/comments?after_id=<id>&limit=<n>` (for a bounded project spike, pair `project=<name>` with `max_task_id=<id>` to scope comments to the selected task prefix)
- `/v1/import/comments?task_ids=<id,...>&after_id=<id>&limit=<n>` (all comments for just those tasks), or `/v1/import/comments?after_at=<RFC3339>&after_id=<id>&limit=<n>` (comments since a `(created_at,id)` cursor)
- `/v1/import/comment-authors?after=<author>&limit=<n>` (distinct author names without loading comment bodies)
- `/v1/import/task-documents?task_ids=<id,...>&after_id=<id>&limit=<n>` (task-linked reports and briefs used for issue comments/backlinks)
- `/v1/import/documents?project=<name>&after_id=<id>&limit=<n>` (`project=/shared` means no task link; `project=/unassigned` means a linked task with no project)
- `/v1/import/documents?project=<name>&after_at=<RFC3339>&after_id=<id>&limit=<n>` (updated articles since a `(updated_at,id)` cursor)
- `/v1/import/document-authors?project=<name>&after_id=<id>&limit=<n>` (lightweight author scan before article import; accepts the same project buckets)
- `/v1/import/decisions?after_id=<id>&limit=<n>`
- `/v1/import/tasks?project=<name>&after_at=<RFC3339>&after_id=<id>&limit=<n>` (task activity and linked report/brief backfill since an `(activity_at,id)` cursor)
- `/v1/import/decisions?after_at=<RFC3339>&after_id=<id>&limit=<n>` (decisions since an `(updated_at,id)` cursor)

Every response is `{ "items": [...], "next_cursor": value|null }`. `next_cursor` is exclusive and stable; the import client continues until it is null. Document pages contain at most 20 full-text rows to bound response size. The endpoint never writes or returns attachment bytes.

## What the client imports

Projects are the configured hk task projects, plus synthetic buckets for unassigned tasks, shared documents, and decisions. The synthetic source selectors use slash-prefixed names that cannot collide with valid task project names. Task and decision issues, task comments, and all text documents carry stable hk source IDs, source authors where present, and source timestamps. Large text documents are split into 240 KiB UTF-8-safe parts with an index article. Binary and large artifacts remain in Drive/R2 and are referenced from text.

When `testProject` is set, preparation scans only the selected task/decision prefix and bounded document-author prefix. Task comments and task-linked report/brief bodies are loaded with the task page that uses them. Full imports preload comment author names without loading every comment body.

`getIssueUpdates` and `getArticleUpdates` use timestamp/ID keysets and page from the previous returned source entity. Task-linked `reports/mwr-*` and `reports/<job>/report.md` documents become stable issue comments; `review/mwr-*` prompts stay in hk; task-linked `brief/<date>/<job>` documents remain articles and add a comment linking their summary and hk key. Report comments split into ordered parts at 240 KiB UTF-8 bytes per comment, matching the article part safety cap.

The first continuous update uses a 2026-11-09 07:17 UTC cursor floor for tasks with linked report or brief documents. The full import completed at 07:16 UTC; this re-sends existing linked documents once, and the persisted update cursor then advances beyond the floor. Existing imported articles are not deleted. To find them in the Knowledge Base, search `tag: hk/doc/reports, hk/doc/review, hk/doc/brief`.

When a split document grows, the importer refreshes its index and sends the new parts under stable IDs. If it shrinks, old excess parts remain because the source API has no delete operation. When a task moves projects, the destination project's update feed resends it under the same hk task ID; the old project feed has no removal signal. Deleting stale parts/articles and reconciling project moves are follow-ups. Agile boards, saved searches and dashboards need YouTrack REST/Hub REST setup; the custom import API does not support those entities.

The import client uses YouTrack's documented custom-import entry points (`getProjects`, `getProject`, `getIssues`, `getUsers`, `getArticles`, `getIssueUpdates`, and `getArticleUpdates`) and the YouTrack scripting HTTP connection API. The target project field types and whether authors/timestamps survive remain runtime acceptance points.
