# Attachment archive: hk attachments to the Google Drive mount

Authority: `decision/2026-11-09/hk-file-gateway` §3, §5, §7 and §8.
Design: `design/2026-11-16/hk-attachment-archive`.

An archived attachment's bytes live in the Drive mount, verified by size and
sha256; hk keeps its metadata and records where the copy is. The hot object in
OCI Object Storage is deleted only after hk has recorded the archive (mode
`on`, below).

## What moves, and when

An attachment is a candidate when every hk task that refers to it is closed
(merged, deployed, completed, dropped or duplicate) and the most recent of
those closes is at least the grace old: 7 days by default, set by
`HANDOFFKEEP_ATTACH_ARCHIVE_GRACE` (below). "Refers to" means a `task` ref or a
`document` ref whose document belongs to a task.

It is never a candidate when it has a ref of another kind (memory, checkpoint,
none, a document of no task), or when an open decision request names its sha
among an option's attachments.

The server answers the candidates; the CLI does not re-derive the rule. See
`GET /v1/attachments/archive-candidates`.

## Layout

```
<archive-root>/
  .hk-archive-root                                   marker, created once by the operator
  fleet-artifacts/archive/tasks/<task-id>/
    manifest.json
    attachments/<sha256>-<safe original name>
```

`<task-id>` is the storage task of the candidate: the lowest closed task that
refers to the sha. A sha several closed tasks refer to is stored once; every
referring task's manifest entry names that one `path`.

`<archive-root>` is the Drive mount on the host that runs the archiver. The
archiver refuses a root that is not an existing directory or that lacks
`.hk-archive-root`, so an unmounted Drive folder never fills the local disk.
The marker is created once, by the operator: `touch <archive-root>/.hk-archive-root`.

## Naming: the safe name

The stored file name is `<sha256>-<safe name>`. `<sha256>` is 64 lowercase hex
bytes, so a file name always starts with the content address and is unique per
sha.

The safe name is the candidate's `original_name` folded to:

- ASCII letters, digits, `.`, `-` and `_` kept as they are;
- every other rune (including `/`, `\`, `:`, spaces and non-ASCII) replaced by
  one `_`;
- cut to 180 bytes (sha256 plus `-` plus this cap stays under the 255-byte
  limit on one file name);
- `attachment` when the result is empty or nothing but dots.

Examples: `report.pdf` → `report.pdf`, `a/b c?.txt` → `a_b_c_.txt`,
`한글.txt` → `__.txt`, `...` → `attachment`.

The path is never derived from the original name alone; the sha prefix keeps
two different names for the same bytes from creating a second copy.

## manifest.json

Each task directory holds one `manifest.json`, with the archive copies that
task refers to in `sha256` order.

| field | meaning |
|---|---|
| `sha256` | content address of the archived bytes |
| `size` | size in bytes of the stored copy |
| `mime` | MIME type hk recorded |
| `original_name` | the name the attachment was uploaded with |
| `path` | archive file location, relative to `<archive-root>` |
| `archived_at` | when the archiver first put the copy in the archive (UTC) |
| `source` | `hk`: the bytes were downloaded through hk's attachment route |

```json
{
  "attachments": [
    {
      "sha256": "3f…64 lowercase hex…",
      "size": 18344,
      "mime": "image/png",
      "original_name": "screenshot.png",
      "path": "fleet-artifacts/archive/tasks/4210/attachments/3f…-screenshot.png",
      "archived_at": "2026-11-15T04:12:09Z",
      "source": "hk"
    }
  ]
}
```

A rerun keeps an existing entry's `archived_at` and rewrites nothing else when
the entry already names the stored path. A manifest that cannot be read is
never replaced: the run reports `manifest_failed` and leaves the file for an
operator.

## Who writes

Only `handoffkeep attachments archive` writes this tree
(`handoffkeep attach archive` is the same command). It runs from the Dagu DAG
`attachment-archive-daily`, on the m1 host when it is connected and on mbp
otherwise, the pattern of the fleet's weekly Drive backup.

Agents and services never write the archive folder (decision §1); they upload
to hk, and the archiver is the one path from hk to Drive. The bulk job and
worktree archives (`fleet-artifacts/archive/tasks/<task-id>/<job>/`,
`no-task/`) use the same manifest idea but a different writer (mergewake's
archive-jobs); this document covers attachments only.

## Verification and order

For each candidate, in this order:

1. Download the bytes with `GET /v1/attachments/{sha}`.
2. Write `<archive-root>/fleet-artifacts/archive/tasks/<task-id>/attachments/<sha256>-<safe name>`
   as a temp file in the same directory, `fsync` it, then rename it into place.
3. Read the stored file back and check its size and sha256.
4. Update the storage task's `manifest.json` and every other referring task's
   `manifest.json` atomically (temp file, `fsync`, rename).
5. Only then call `POST /v1/attachments/{sha}/archive` with the location
   relative to `<archive-root>`, the size and the sha.

A copy that fails its read-back is removed so a later run retries it; it is
never recorded. A stored file whose bytes differ from the candidate is not
overwritten and not recorded; the run reports `file_mismatch` for that sha and
continues with the next candidate.

The run is idempotent:

- a stored file with the right sha is not downloaded or rewritten;
- recording the same location again answers the recorded result;
- a run that stopped between the copy and the record converges on the next
  run: the copy is found present, the manifest is left as it is, and the
  record is made.

## Command

```bash
handoffkeep attachments archive --root <archive-root>            # dry run
handoffkeep attachments archive --root <archive-root> --apply    # copy, verify, record
handoffkeep attachments archive --root <archive-root> --apply --limit 20
```

The dry run lists every file it would write and writes nothing. `--limit N`
processes at most N candidates; without it the run pages through all of them.

Output is one JSON summary:

```json
{"dry_run":false,"candidates":12,"copied":9,"already_present":3,"recorded":12,"failed":[{"sha256":"…","task_id":4210,"reason":"file_mismatch","detail":"…"}],"planned":[…]}
```

`planned` appears in a dry run and lists the files it would write. A `failed`
entry is never recorded. Reasons: `candidate_refused`, `download_failed`,
`write_failed`, `verify_failed`, `file_mismatch`, `manifest_failed`,
`record_failed`.

Each action also logs one line: `attachment_archive.copied`,
`.already_present`, `.manifest_written`, `.recorded` or `.planned`. A candidate
that stops logs `.candidate_failed`, `.download_failed`, `.write_failed`,
`.verify_failed`, `.manifest_failed` or `.record_failed`.

## Mode and the hot copy

`HANDOFFKEEP_ATTACH_ARCHIVE_MODE` decides what the server does with a record:

- `off` (default): the archive endpoint refuses.
- `record`: hk records the archive and keeps the hot object. This is the
  canary: reads keep working while the Drive copy is checked.
- `on`: hk records the archive and then deletes the hot object. The delete runs
  after the record commits; a failed delete is retried by the next call and
  reported, never silently ignored.

`HANDOFFKEEP_ATTACH_ARCHIVE_GRACE` (read at serve start, next to the mode) sets
the grace: a Go duration such as `24h` or `168h`. Unset means 7 days. A value
that does not parse, or is below `1h`, stops the server at start. It moves only
the candidate cutoff; the rule is otherwise unchanged.

Once recorded, reads of the attachment answer `410 attachment_archived` with
the location and sha on the download route, say "archived at `<location>`" in
`attachment_read` and `attachment_get_url`, and the console shows the file as
보관됨 with its location. Metadata (name, size, mime, sha) stays listed. A
restore (re-upload from Drive) is a later task, not built here.

## Rollout order

1. Deploy the server routes and the client (S1), then this CLI (S2).
2. Run a dry run on mbp with the Drive mount and compare `planned` with what
   the candidates should be.
3. Set `HANDOFFKEEP_ATTACH_ARCHIVE_MODE=record`, apply a few candidates, and
   check the download route, `attachment_read` and the console against them.
4. Set the mode to `on` and let the hot deletes run.
5. Register `attachment-archive-daily` in Dagu (m1 first, then mbp, a timeout
   and continue-on-failure per host, idempotent by marker).

hk-env changes and the DAG registration are operator commands.
