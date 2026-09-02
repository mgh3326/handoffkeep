# Checkpoint contract

Write a checkpoint every 30 minutes, before a risky operation, and before a
session ends. A resumed session starts with `ctx recent` for its source
session. Checkpoints are short, factual handoff records: state, decision,
next action, and safe references to normal Git or job artifacts.

The service rejects credential-shaped text before it is persisted. Never place
tokens in checkpoint, memory, or document bodies. Each write records the
authenticated client ID; callers cannot choose that value.

Use repeatable `--ref key=value` flags to attach safe references. They are
stored and returned as JSON arrays (for example `"prs":["26"]` and
`"jobs":["hk-v0"]`) in checkpoint and checkpoint-search output. Memory push
and pull preserve the complete Markdown file byte-for-byte for new records,
including frontmatter, body, and final newline.

Limits are 64 KiB per checkpoint/memory field, the newest 500 checkpoints per
session, and 2,000 memories per agent. Documents are 512 KiB, use SHA-256
idempotency, and are not a substitute for source control.
