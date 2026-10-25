# hk 1873 piece 1: server authority for wrk claims and rounds

Author: b1923-sol (continuing b1873a-sol). Task: hk 1923; specification: hk 1873. Revision 4, 2026-10-24. Profile builder-sol, effort xhigh, model gpt-6.1-sol, T2, S. Wall-clock start: 2026-10-24 09:06:29 UTC (18:06:29 KST). Review and finish times are recorded in the delivery report.

This is a proposed implementation contract, not deployed behaviour. Current-code assertions are distinguished from the proposed contract and cite the pinned heads. Citation aliases resolve to immutable heads:

- AS = agent-skills c9a5691dbd32b6976d649838af5727b33394f643, origin/main at start, read in the supplied worktree.
- HK = handoffkeep 1ddad8579c5bca41739e3df2bb8ed2256a472b6a, GitHub main read from its immutable commit archive; the local checkout was not used as the baseline.
- PW = panewire 4365ca93f0b613f22a1ea238bd4ddc6bc4431a70, GitHub main read from its immutable commit archive.
- AS242 = agent-skills PR 242 head bf740c3c33e12321185fcaffb27171d5f0450068, read from its immutable commit archive. PR 242 was OPEN, mergeCommit=null, at this read; remote main was still AS, which already contains hk 1449. This document does not claim PR 242 was merged. C1 and D2 must reconcile the eventual merged PR 242 before implementation/cutover. No source checkout was changed.
- Input provenance: revision 3 is doc 43850, sha256 a871361c7eb0d0a3307bd0c8042bbb549af91c542560d37e6eb1aa7b64cb3f1c; adoption review is doc 43983, sha256 61de3d067ddbe614fa2eb47458e6de23f26c28b2a9d7bbcba1cf1e568afddf56. Operator packet is hk:doc decisions/hk1922-o3-operator-questions, doc 44050, sha256 a7a8ddaf66f44ef2950556cf7ff0eb325b2364663ae9fce29d9f3349dad08f57. hk 1922 dr-1922-1 revision 1 was answered A at 2026-10-24T07:25:47.771697849Z, task event 5657; operator-desk comment 7044 and director-1 comment 7046 confirm Q1=a through Q13=a and desk's Q4/Q12 commitments. All are decided in §10.
- Publication: the authoritative deliverable for this brief is hk:doc designs/hk1873/b1873a-sol-r4, the revision-4 successor in the same design family. The original hk:doc designs/hk1873/b1873a-sol remains revision 3 unchanged, as explicitly required by the outer builder brief.
- Policy evidence: hk:doc decisions/hk525-root-redirect-packet, sha256 31230c79746c6f6bdc75ee3aac8c4f8d0b161f78b396ffe89e62d395720b4988; hk 525 decision dr-525-1 answer A and comments 6741–6742; hk 1398 state merged at 12:21:17 KST and desk comments 2331, 2335, 6989. The prerequisite was checked before design work. hk 1901 is a design request whose head_repeat contract is now approved by Q2=a; implementation is still required. The director's addition about at/pb/tt is included below.

## 1. Threat model

The actor is a misbehaving agent builder or tester that ignores its brief, mistakes authority, or optimizes for done. It is not an untrusted human adversary. Assets are the tester round cap; authority to grant extra rounds, voids and exceptions; the job's parent lane/task binding; and immutable completion evidence used by the director and quota-release consumers. An agent can edit its own binaries, events, local arbiter database, paths and files, and can use any credential reachable by its OS user. A host label, file mode, process name or claimed agent identity is not a privilege boundary.

Current: arbiter paths can be selected by caller environment (AS bin/arbiter:79–91); wrk reads the events root from the caller (AS bin/wrk:4895–4899), reads parent/role from the latest local claim/reclaim (AS bin/wrk:4901–4919), and selects the grant-fetching binary through a caller-controlled lookup (AS bin/wrk:5612–5622). Generic arbiter event now refuses claim kinds, but arbiter claim still mints local claim/reclaim records (AS bin/arbiter:645–717,1420–1434). These facts describe AS. PR 242 changes authority reads: it derives the canonical home from the OS account; pins inbox, arbiter DB and handoffkeep paths; rejects redirection outside those paths; checks the real binary is regular, owned by the invoking uid and not group/world writable; and routes grant/round-extension/takeover reads through that guard (AS242 bin/wrk:4903–5118,5858–5871,9974–9988,10358–10367; AS242 bin/arbiter:94–312). The unguarded general hk_bin helper remains at AS242 bin/wrk:9888–9900: C1 must NOT use it for O3. Every new CLI call uses the guard-selected canonical binary (§6b), preserving hk 1454 and PR 242 controls on reads not replaced by O3.

Proposed boundary: the server owns claims, the journal, counters, grant consumption and state transitions. Only director-1 and operator-desk can create root/builder/worker claims, claim corrections, successors and grants (Q7=a, Q1=a). An authenticated host may create only the constrained review assignment claim in §2 (Q13=a), and submit allowed evidence for assigned jobs. That assignment has no grant or review budget of its own. No other server write path, local event, mutable hk document or task ref can synthesize journal authority. A director independently checks server receipts before accepting completion or releasing quota. A locally replaced wrk can display success, but cannot create a matching accepted server transition.

| Host client id / role id | What enforced O3 bounds, assuming no privileged token is reachable there | What remains narrowed or unbounded |
|---|---|---|
| desktop | Parent/task/host binding, cap, grant issuance/consumption, cross-host writes, accepted record integrity and join gate | Host agent may submit invented verdict/report bytes for its own jobs; individual tester authorship and actual review effort are unbounded within that OS user. Local execution cost outside the protocol is unbounded. |
| mac-dev | Same server bounds as desktop | Same host evidence/authorship limits; any co-resident agent can use this host's token. |
| mac-work | Same server bounds as desktop | Same host evidence/authorship limits. |
| rpi | Same server bounds as desktop | Same host evidence/authorship limits. |
| mac-personal on mbp | A mac-personal request by itself can mint only the scoped review assignment and cannot issue root/builder claims or grants; server records remain append-only | mbp builders, director-1 and operator-desk wrappers share one OS user. Any mbp agent able to use their tokens can exercise their authority. Grant forgery, fabricated claims and privileged exceptions remain unbounded against that agent; O3 narrows the explicit route and records the credential identity. It does not bound mbp. |
| director-1, operator-desk on mbp | Server attributes writes to the authenticated role id and enforces exact grant consumption | Role attribution does not prove which pane used the wrapper. Same-OS-user access is unbounded; no O4/user separation is proposed. |
| pa-integrator on mbp | Dedicated client attribution; no O3 grant/claim issuance permission in this design | Same OS user can reach higher-authority wrappers. Parent-lane identity is routing data, not grant authority. |

Current attribution: at, pb and tt write as mac-personal on mbp (hk 1398 comment 6989). Decided Q4=a: A1/hk 1928 adds the explicit ids at-integrator, pb-integrator and tt-integrator with dedicated wrappers and read-only O3 permissions, no grant/claim scope. Desk issues and installs the three credentials when hk 1928 is ready; this task handles no credential material. Before that installation their writes remain mac-personal. Thereafter the server records the three distinct ids; they remain unbounded against access to co-resident privileged credentials on mbp. Names and wrappers give attribution, not an OS-user boundary. Unknown client ids (including any newly deployed host) get no O3 permissions until operator policy explicitly assigns them.

Bounds apply only after server enforcement AND director/consumer receipt enforcement. Shadow dual-write is O2 with detection. Server compromise, database administrator access and loss of all durable storage are outside this actor model; recovery must nevertheless detect acknowledged history regression (§8).

## 2. Data model

Current: handoffkeep uses PostgreSQL and additive migrations serialized by an advisory transaction lock (HK internal/store/store.go:561–595). Generic documents are mutable upserts, and an update replaces created_by (HK internal/store/store.go:2941–2960). They are unsuitable as authoritative append-only grants.

Use new tables, not documents or relay_events as the authority. Every key below is exact, case-sensitive UTF-8 except normalized lowercase SHA values. UUID means canonical lowercase UUID text on the wire and UUID in PostgreSQL. All integer wire values are in 0..9007199254740991. Server time is UTC RFC3339 with microseconds; order is seq, never timestamp.

1. wrk_job_locks: job_id TEXT PRIMARY KEY. A stable lock row only; SELECT FOR UPDATE serializes every append to that job. No mutable authority fields. Lock creation and the first claim share the transaction.
2. wrk_records: PRIMARY KEY(job_id,seq), record_id UUID UNIQUE, store_generation UUID, kind TEXT, claim_seq BIGINT, actor_client_id TEXT, idempotency_key UUID, request_sha256 CHAR(64), payload JSONB, imported BOOLEAN, source JSONB or NULL, received_at TIMESTAMPTZ, previous_hash CHAR(64), record_hash CHAR(64). UNIQUE(actor_client_id,job_id,idempotency_key). kind is exactly claim, activate, round_open, verdict, void, joined, completed, grant, grant_cancel, end, import, import_begin or import_seal. seq starts at 1, is allocated under the job lock, and is gap-free for committed records. For a new non-import record imported=false and source=null; import is always imported=true. received_at/actor/digests are server-owned, never accepted from requests. record_hash = SHA256 of RFC8785 canonical JSON of all record fields except record_hash; previous_hash is all zeroes for seq 1. Payload never includes credential material.
3. wrk_contents: PRIMARY KEY(job_id,seq), FK to wrk_records, content BYTEA, content_sha256 CHAR(64), size_bytes INTEGER. Immutable verdict/completed/joined/import bytes, written in the same transaction as their record; 1..262144 bytes. Do not reference a mutable document key as evidence. Ordinary reads list metadata, and a separate content read returns bytes. No application UPDATE/DELETE privilege on records or contents. Migration owner and runtime DB roles must be distinct; rollbacks preserve these tables.
4. wrk_attempts: attempt_id UUID PRIMARY KEY, actor_client_id TEXT or null for unauthenticated requests, job_id TEXT or null, kind TEXT or null, stable_error_code TEXT, received_at TIMESTAMPTZ. Server audit only, without body, path, request credentials or task text. Indexed(job_id,received_at,attempt_id). Rejected writes append an attempt, never a successful business record or grant consumption. Audit persistence failure must not convert a denied operation to success.
5. Server-side nonsecret auth policy file: the exact Policy schema in §5, including client permissions, root lane edges and feature switches; loaded as an immutable validated snapshot. Not stored in a host-writable document. Its revision digest is exposed by capabilities and captured on claim creation.

Exact kind payloads are the request schemas in §5, with these output rules:

| Kind | Persisted exact payload fields and constraints |
|---|---|
| claim | job_id, owner_lane, parent_lane, task_id, host_client_id, role, tier, review_of, predecessor_job_id, supersedes, policy_sha256, created_at. job_id equals path; task_id positive. role builder/worker/tester/reviewer/counsel. review_of is null or {job_id,round_no}; non-null only for tester/reviewer/counsel and same task as target. supersedes is null for first claim, otherwise the prior canonical claim seq. policy_sha256 and created_at server-set. Scoped review-role claims use the exact derived-field/assignment constraints below. |
| activate | baseline_used, mode="enforced", seal_seq (nullable), reason_code. No automatic downgrade. baseline_used is computed by the server as described below, not accepted as a smaller client value. |
| round_open | round_no, head, tester_job_id (nullable), extension_grant_seq (nullable), repeat_grant_seq (nullable). round_no server-assigned as 1+maximum prior authoritative or valid imported round_no and inherited numbering floor, including voids; never recycled. |
| verdict | round_no, head, verdict, path, content_sha256, size_bytes, tester_job_id (nullable). head/verdict/hash/size are server-parsed/computed from stored bytes; submitted assertions must match. path is normalized as below. |
| void | round_no, grant_seq, reason_code. At most one void for a round; no verdict may exist on it. |
| joined | head, pr, report_sha256, report_size_bytes, verdict_seqs, waiver_grant_seq (nullable). Bytes stored in contents as for completed; request report bytes become digest/size. verdict_seqs is server-derived, complete for all nonvoid rounds; a waiver may replace only the zero-round prerequisite, not hide BLOCKER/open rounds. |
| completed | report_sha256, report_size_bytes, warnings (server-computed string array). Observation of report submission, never a PASS or join. |
| grant | grant_type, round_no (nullable only for join_waiver), head (nullable only for void), extra, earlier_round_no (nullable), tester_job_id (nullable), expires_at, reason_code. Types extension/void/head_repeat/join_waiver. extra is 1 for extension and 0 otherwise. All grants bind job_id via record key AND current claim_seq; no wildcard. |
| grant_cancel | grant_seq, reason_code. Only unconsumed grants can be cancelled. |
| end | end_type (revoked/reaped/handed_off), reason_code. No revive of this job id. |
| import_begin | reason_code, shadow_cutoff_seq; freezes host writes, central only. |
| import_seal | begin_seq, expected_import_count, manifest_sha256, reason_code, shadow_cutoff_seq, baseline_used, numbering_floor; pins exact imported snapshot. |
| import | source_locator, source_sha256, original_actor_client_id (nullable, ASSERTED only), original_at (nullable, ASSERTED only), legacy_kind. Archived content bytes go in wrk_contents, not payload. Server verifies source_sha256 over decoded archived legacy event bytes. This is an imported legacy record, not a trusted verdict/grant/claim. |


Scoped review-assignment claims (Q13=a, also used by Q9=a): the wire kind remains claim and the record model is unchanged. A host with reviewer.claim.write may submit Input_claim only for a new job id with role tester, reviewer or counsel and non-null review_of. The reviewed job must be an active, centrally assigned builder/worker job (state active, not joined/ended/importing/sealed), and its canonical claim.host_client_id must equal the authenticated id. The new claim's job_id must differ from review_of.job_id and never have been used. The server requires exact derived values: task_id=reviewed claim.task_id, host_client_id=authenticated id, tier=reviewed claim.tier, owner_lane=reviewed claim.owner_lane, parent_lane=reviewed claim.owner_lane, predecessor_job_id=null, supersedes=null. review_of.round_no must equal the target's next_round_no or an existing live open round; it cannot replace an already frozen tester assignment. The host chooses only the fresh job id, allowed review role and eligible review_of round; every other field is pinned to the values above. Added grant fields are unknown fields and invalid_request. Any inconsistent pinned field is claim_conflict, except host identity violations are host_mismatch. Root/builder/worker claims, corrections and successors remain central-only.

A review assignment's parent_lane routes to the reviewed builder's lane. Its owner_lane names that same existing lane for ownership; this pair is NOT a new owner-to-parent graph edge. It cannot create or reparent a lane, supersede the builder or rebind the task. This role-specific lane rule applies to centrally minted review claims too; a central role may place such a reviewer on a different host, but a host may not. Other worker claims retain the existing lane graph rule.

Assignment activeness is precise: a canonical, unsuperseded review-role claim with non-null review_of and no end is a live assignment while its reviewed subject is active. During P1 only, a centrally minted assignment for a shadow subject is live for shadow validation/observation, never for a trusted joining PASS. The review-role job's own claim-only journal may remain summary.state=shadow; that state tracks budget/import progression, NOT its lifecycle protection. No separate activate is needed or allowed for a review-role job. It cannot round_open, void or joined (scope_denied), mint another assignment, issue grants, or inherit/reset an independent cap budget. The reviewed subject's later round_open is the only budget charge. Accepted PASS survives later reviewer cleanup as before.

Canonical review-role lifecycle protection (R4 round-1 B1 closure): receipt_required(job) is true if that job has an activate record OR any accepted canonical claim in that job's claim history is a tester/reviewer/counsel claim with non-null review_of. The second case takes effect atomically with claim acceptance, including a central P1 assignment. It is monotonic for that job id: subject join/end, reviewer end, policy flags and summary.state=shadow never turn it off. No new record kind, payload field, state or activate event is introduced. Every server, C1, C2, legacy relay route and cleanup consumer computes this same predicate from canonical records; a local enrollment file or summary.state alone is insufficient. For such a review-role job, completed and end are canonical lifecycle records, NOT mere preactivation observations. They authorize that reviewer's own accepted completion/cleanup and end its assignment liveness respectively; they do not establish target PASS, alter target used/baseline or authorize target task closure. Every own-job write must receive a canonical receipt before local completion/event, relay, housekeeping, counting or quota release. Unavailability/5xx/unknown outcome is exit 75 with a pending intent and no success event/relay/release. A receipt-less legacy job.completed/job.joined/end publication for this job returns wrk_receipt_required. End remains final and exact replay remains available. Review-role jobs cannot use import_begin/import/import_seal/activate to evade this lifecycle rule; return scope_denied. A central P1 claim creates the same protected lifecycle even though the subject's review observations remain O2; P1 is not a fleet durability/security-bound claim before Q12.


Indexes: wrk_records(job_id,kind,seq); (actor_client_id,received_at,record_id); expression index on claim payload task_id; (job_id,(payload->>'round_no')) for round_open/verdict/void; UNIQUE(job_id,(payload->>'round_no')) separately WHERE kind=round_open, verdict or void; UNIQUE(job_id,(payload->>'path')) WHERE kind=verdict; UNIQUE(job_id,(payload->>'content_sha256')) WHERE kind=verdict. Imported legacy payloads do not populate authoritative indexes. UNIQUE(job_id,(payload->>'grant_seq')) WHERE kind IN (void,grant_cancel), plus transactional consumption checks across all consumers. One grant can be consumed once: extension_grant_seq/repeat_grant_seq/waiver_grant_seq/grant_seq are indexed individually and checked together under the same job lock. A grant type has only one legal consuming field, so these per-type unique indexes enforce the same fact. UNIQUE((payload->>'predecessor_job_id')) WHERE kind=claim AND (payload->>'supersedes') IS NULL AND (payload->>'predecessor_job_id') IS NOT NULL prevents successor branching. Cross-job predecessor locks are acquired in lexicographic job order.

Path identity is POSIX lexical normalization of an absolute path supplied by the submitting host: collapse repeated slash, dot and dot-dot, reject NUL/control characters, backslash, nonabsolute paths and length over 4096 UTF-8 bytes; preserve case and Unicode bytes, do not resolve symlinks or access host disks. UNIQUE(job_id,path) is intentionally stricter across hosts; a fresh per-round artifact path is required. Verdict content identity is global within the job; completed/joined report bytes may equal verdict bytes, but never substitute for the separate required verdict record. In active mode, a head held by an earlier nonvoid post-activation open or any actual verdict requires one explicit head_repeat grant and still consumes another round. A verdict-free void releases that open's head reservation; path/content identity from actual verdicts is never released. No unique head index: stateful replay checks under the lock provide that rule.

Append-only correction: an erroneous claim can be superseded by another claim only before activation and any authoritative round/grant/completed/join/end. Claims and their original created_at stay visible. After activation, wrong ownership/task/parent requires privileged end and a centrally minted successor with predecessor_job_id, inheriting all used slots. No reset by reclaim, new head or new owner. Erroneous verdicts are never edited/deleted or voided; correct them by a new authorized round and head_repeat grant if the head is reused. Unused grants may be cancelled and replaced. Terminal mistakes require a successor, not resurrection. A latest-per-job projection is a disposable cache, never an alternate authority.

## 3. Authorization matrix

Current: legacy auth maps HANDOFFKEEP_TOKEN_<client> lines to ids, compares bearer strings in a plain loop, and stamps document identity from that id; there is no scope carried by Tokens (HK internal/api/api.go:367–406,203–205,519–524). Preserve that auth file grammar unchanged. Add a separate nonsecret, server-owned JSON policy file keyed by authenticated id. Missing, invalid or unknown policy denies all O3 operations. Duplicate client ids or equal token values must make new server startup fail; otherwise role attribution is ambiguous. Use fixed-length digest comparison for token lookup as defence in depth; this is not the new authority boundary.

No builder/tester bearer kinds are introduced. The Q4 attribution ids are not new authority roles. Sharing such tokens under one OS user would not prove reviewer independence. Each host keeps its existing id. No request can supply its actor id or effective permissions. The fixed matrix is an upper bound: policy can attenuate these permissions but cannot widen them or map a host to another host id. Unknown permission names fail policy validation and keep the O3 API unavailable; unknown authenticated ids have no O3 permission. The precise policy grammar, lane rules and canonical hash are in §5. Legacy endpoints remain compatible for unrelated jobs/documents; O3-sensitive legacy writes are fenced as below.

Legend: D=deny; A=allow all jobs under central authority; H=allow only canonical claim.host_client_id equal to the authenticated id (for reviewer claim minting, H refers to the reviewed job and all §2 restrictions apply); V=allow evidence for own H job or a target round assigned by a server-accepted tester/reviewer/counsel claim on this host, with review_of and same task matching exactly; R=read all O3 job metadata/content (fleet operational data, no credentials); S=server internal only. There are no empty cells. A read is permission to inspect, not permission to mutate.

| Operation | desktop | mac-dev | mac-work | rpi | mac-personal | director-1 | pa-integrator | operator-desk | at-integrator / pb-integrator / tt-integrator |
|---|---|---|---|---|---|---|---|---|---|
| Read capabilities | R | R | R | R | R | R | R | R | R |
| Read canonical claim/summary | R | R | R | R | R | R | R | R | R |
| Read rounds/verdicts/voids/joined/completed/grants/end/import records, paginated | R | R | R | R | R | R | R | R | R |
| Read immutable verdict/report content | R | R | R | R | R | R | R | R | R |
| Write claim or claim correction | D | D | D | D | D | A | D | A | D |
| Write reviewer claim for own job | H | H | H | H | H | A | D | A | D |
| Write activate | D | D | D | D | D | A | D | A | D |
| Write round_open | H | H | H | H | H | D | D | D | D |
| Write verdict | V | V | V | V | V | D | D | D | D |
| Write void using an existing valid void grant | H | H | H | H | H | D | D | D | D |
| Write joined | H | H | H | H | H | D | D | D | D |
| Write completed | H | H | H | H | H | D | D | D | D |
| Write any grant type | D | D | D | D | D | A | D | A | D |
| Write grant_cancel | D | D | D | D | D | A | D | A | D |
| Write end | H | H | H | H | H | A | D | A | D |
| Write import_begin | D | D | D | D | D | A | D | A | D |
| Write import_seal | D | D | D | D | D | A | D | A | D |
| Write imported legacy record | D | D | D | D | D | A | D | A | D |
| Write audit attempts / compute receipt fields | D | D | D | D | D | D | D | D | D |
| Read attempt audit | D | D | D | D | D | D | D | D | D |
| Legacy relay publication with valid canonical receipt | H | H | H | H | H | D | D | D | D |
| Legacy protected task claim/ref/rebind mutation | D | D | D | D | D | D | D | D | D |
| Legacy protected task state-only mutation with canonical receipt | D | D | D | D | D | A | D | A | D |

Audit writes are S and have no public endpoint. Audit inspection is deployment-operator tooling outside this API; it must not be implemented as a new permissive general read route. Unknown ids, and missing/invalid bearer tokens, have no permissions. at-integrator/pb-integrator/tt-integrator have the explicit attribution-only column; until desk installation, at/pb/tt use mac-personal. The mbp limitation in §1 applies to all.

The scoped reviewer mint uses reviewer.claim.write; it does not use claim.write and cannot reach its central branch. Central A permissions for claims do not authorize arbitrary contradictory state: server validates task existence, binding and predecessor lineage; validates parent/owner against the configured lane graph; and atomically binds builder tasks. It inherits tester task identity from review_of instead of claiming the task again. A host cannot create a root claim by writing tasks.refs.job_id, changing project, posting a document, choosing a lane, or replaying an arbiter event. For O3 jobs, only the canonical claim determines the parent/task. director-1 has fleet supervisory authority; pa-integrator must request grants from director-1 or operator-desk even for ParkingArcade. This deliberately tightens the current operator/pa-integrator issuer set (AS bin/wrk:5413–5425,5669–5686,5726–5743) to the brief's two allowed grant writers.

## 4. Server-side rules

Current: local round count includes opens, unbound verdicts and inherited takeover slots; a valid verdict-free void removes a slot (AS bin/wrk:4531–4570,5564–5606). Local verdict identity checks path, exact content and head in that order; a historical void can release head identity even though command void rejects rounds with verdicts (AS bin/wrk:6037–6054,6138–6187). A verdict currently binds to the oldest open, without comparing its head to that open, or becomes an unbound counted round (AS bin/wrk:6188–6207). O3 uses explicit round_no and exact head matching and rejects unbound verdicts. Imported unbound verdicts are preserved and conservatively counted, not silently accepted as fresh authority.

Every mutation transaction locks the job, authenticates using server policy, checks caller permission without rechecking mutable target activeness on replay, checks successful idempotency replay BEFORE expected_seq, expiry, terminal state and business rules, then checks expected_seq and claim_seq, validates canonical claim/host and state, appends record/content/receipt and commits durably before sending 201. Authentication/permission is rechecked even on replay; revoked credentials cannot retrieve old responses. An exact successful replay returns 200 and the original record, even after end or grant expiry; it never appends again. A reused successful key with a different canonical request returns idempotency_conflict. Failed attempts do not reserve a key. JSON canonicalization is RFC8785; duplicate object keys and unknown fields are invalid_request, never last-key-wins.

- Cap: baseline_used + count(authoritative round_open without a void) must be less than 3 before an ordinary open. Once at/above 3, exactly one unused extension grant for this job/claim, proposed next round and head must be consumed with the open. Before cap, an extension is rejected. An extension is never refunded by a void; the void merely reduces used by one. Counter snapshots, open allocation and all grant consumers serialize on the job lock. A head/host/owner change never resets the count.
- Identity: a verdict requires an existing nonvoid open, no earlier verdict for that round, exact normalized head equality, and the assigned tester id when the open supplies one. Server computes SHA256 over exact decoded bytes and applies the exact verdict grammar in §5. SHA and verdict assertions must match. Path reuse and byte reuse are refused for the entire job, including earlier ended/predecessor history when inherited; never release these identities. In active mode, head reuse at round_open requires head_repeat when that head is reserved by a nonvoid POST-ACTIVATION open or has an actual historical/live verdict. Historical preactivation opens never reserve a head after activation, even without a void; only historical verdicts retain negative head identity. A verdict-free void removes only its own reservation; an older verdict on that head still requires head_repeat. Grant binds one later round, head and earlier_round_no, and cannot release path/content identity. The consuming round_open.tester_job_id MUST be non-null and exactly equal grant.tester_job_id; a later verdict must name that same id. The reviewer claim must be active at round_open when named and again at verdict append. These are acceptance-time checks; later reviewer end/reap never retroactively invalidates an accepted verdict. Cross-job target/reviewer/predecessor locks use lexicographic job id order, including when a reviewer end races a verdict. Q2=a adopts the explicit hk 1901 exception (§8); head_repeat_enabled must be true, and each repeat still needs its typed grant.
- End: any end record is final. No new record of any kind is allowed afterward, including grants/imports; read and exact successful replay remain possible. joined seals review operations; only one later end is allowed. completed is observational, not an end, so builder done can precede joined. New recovery work uses a privileged successor and inherited count.
- Join: reject zero nonvoid authoritative rounds without a current, one-use join_waiver bound to this head/claim. Reject pending open rounds and unresolved BLOCKER for the joining head; require a PASS on that exact head from the latest nonvoid round for it, and no later BLOCKER on any head. Later/earlier for resolving verdicts means round_no order, independent of verdict arrival seq; pending rounds always block join. A newer PASS on the joining head resolves prior BLOCKER rounds; all history remains visible. A waiver only permits a true zero-round join and never skips an existing open, BLOCKER or evidence mismatch. Every required verdict must have server-stored bytes. For a true zero-authoritative-round join with a valid join_waiver, the PASS and reviewer-assignment requirements are explicitly waived; joined records the waiver and makes no claim that testing occurred. The waiver cannot suppress existing rounds or BLOCKERs. Otherwise, for frozen claim tiers T2/T3, a satisfying PASS must name a server-accepted tester/reviewer/counsel claim matching that target round that was active when the verdict was appended; a null tester id is review_incomplete. Reviewer activeness is checked at round_open when the open names that reviewer and at verdict append, never rechecked at joined. After a PASS is accepted, a later end/reap of that reviewer cannot invalidate the evidence or veto the builder's join. Join checks the frozen reviewer assignment as it existed at verdict acceptance, regardless of the reviewer's later state. This narrows assignment discipline without claiming same-user authorship isolation. Imported legacy PASS is never sufficient. pr must be an HTTPS URL with no userinfo; server stores it as evidence, not proof of CI or remote Git state.
- Grant validation: authenticated issuer must be director-1/operator-desk. Frozen job, claim, action, round/head, required tester_job_id, expiry and extra are exact. Every grant authorizes one consuming append; no multi-extra grants and no string-only hk:doc/hk:task authority. Historic approval refs may be cited in reason evidence outside the journal, but must be reissued as typed server grants by an authorized role. Mutable document identity cannot become authority.
- Claims: root claim must be centrally minted before host work. Successor inherits the predecessor's whole effective budget and its path/content/head identity set, including baseline, with the predecessor ended; no branching, cycles or task change. Server uses current task binding only during privileged claim creation/transfer, then freezes it. Legacy rebind cannot change that journal binding.
- Import activation: shadow imports carry no authority. At activation, baseline_used is max(3, number of archived legacy job.round plus number of archived unbound job.verdict plus verified inherited slots) for an in-flight imported job. Ignore imported void reductions. The full freeze/seal/activation state machine below is normative. Missing/malformed history sets the floor to at least 3 and a migration warning. This means fresh rounds require centrally issued extension grants, even if local files were pruned. For a genuinely new claim with no work evidence/predecessor, baseline_used=0. For a trusted O3 successor it is predecessor.used. A server-shadow history with no independent provenance is treated as legacy import, not zero. An activation can never lower a previously committed budget. Only post-activation opens/voids affect live pending rounds or decrement live used; preactivation evidence is permanently historical.

Client-side checks remain: Git commit existence and bounded fetch (server has no host Git object store; current AS bin/wrk:5493–5558), regular-file reading and path generation, unmatched local verdict scan (server cannot see unsubmitted files; current AS bin/wrk:5050–5088), independent reviewer family/effort/CI/base verification, and routing/notification. They remain O2 discipline and cannot prove absence of hidden local effort. Director gates must use journal records, not a claim that the client ran these checks.

Legacy bypass closure is mandatory for enforcement: current APIs expose task claim/rebind and generic relay publication (HK internal/api/api.go:431–454; HK internal/store/store.go:1893–1961,2479–2540). For protected-task state changes the receipt must name that task's CURRENT canonically bound builder/worker subject job and current claim, not merely another job sharing task_id. A review-role claim, completed or end can never close/rebind/drop the subject task. For a protected task, noncentral legacy calls that mutate job binding, parent, claimed_by or terminal state return wrk_receipt_required, including no-job exceptions, tasks next selection, transition refs and MCP/UI/service entrypoints. Central binding changes must use the new claim/transfer transaction; a standalone legacy rebind also returns wrk_receipt_required. For any job satisfying §2 receipt_required, including a never-activated canonical review-role job, existing relay job.completed/job.joined/end publications require a canonical record receipt and exact record/job/kind/head/report digest match. Their relay projection is inserted atomically with canonical completed/joined and later local notification retries merely deduplicate it. Other text lane events remain notifications and never completion authority. Consumers must fail closed when a job/task lacks a receipt; deleting local enrollment markers cannot make a protected task legacy.

All error responses have precisely {"error":"stable_code"}, application/json, no echoed task text, paths or database error. This matches the stable error style of auth/appErr (HK internal/api/api.go:519–529,580–590) without using its optional textual reasons. Exact mapping:

| HTTP | Stable codes (one error field only) |
|---|---|
| 400 | invalid_request, invalid_cursor, invalid_verdict, invalid_path, grant_before_cap |
| 401 | unauthorized |
| 403 | scope_denied, host_mismatch |
| 404 | not_found |
| 409 | idempotency_conflict, sequence_conflict, claim_conflict, job_ended, review_sealed, round_cap, round_not_open, round_has_verdict, round_voided, head_mismatch, duplicate_path, duplicate_content, duplicate_head, grant_invalid, grant_consumed, grant_expired, zero_rounds, review_incomplete, blocker_unresolved, wrk_receipt_required, import_unsealed, import_sealed, migration_frozen |
| 413 | request_too_large |
| 405 | method_not_allowed |
| 415 | unsupported_media_type |
| 503 | wrk_unavailable, wrk_recovery_required |

If multiple business checks fail: after idempotency/CAS, use end/sealed, grant validity, round existence/assignment, verdict parsing/assertions, path duplicate, content duplicate, head duplicate, then cap/join rules. Error attempts are auditable; denial creates no business record.

## 5. API and machine-checkable contract

New namespace /v1/wrk. No DELETE/PATCH. Bearer auth uses the existing transport; no token is supplied in JSON. All writes use Content-Type application/json, required Idempotency-Key UUID, and request {kind,claim_seq,expected_seq,payload}. The request union below is closed. claim_seq is 0 on first claim, otherwise current canonical claim seq; persisted record.claim_seq equals its own seq for a claim and otherwise the current canonical claim seq; expected_seq is latest observed journal seq (0 before first claim). Server-created claim corrections return a new claim_seq. All success bodies and read summary state refer to the same committed snapshot. Clients derive lifecycle receipt_required from canonical claim role/review_of history plus activation history, never summary.state alone. A successful replay retains its original snapshot; fetch the current canonical history before another lifecycle decision. The claim-role semantic rule in §2/§5 adds reviewer.claim.write dispatch without introducing a new record kind; review assignment liveness is distinct from the reviewer journal summary state. Wire versions are selected by the namespace plus contract string hk1873.v1; incompatible change requires v2.

GET capabilities returns 200, the contract, store_generation, authenticated client id, policy digest and whether writes are available. Unknown/unauthorized clients fail as in §3. It does not expose tokens. GET job returns canonical claim and summary. GET records supplies ordered, snapshot-consistent pagination: first request after_seq defaults 0, limit defaults 100/max 500; snapshot_seq defaults current max. Response includes snapshot_seq, next_after_seq (last returned seq; unchanged for an empty page), has_more and record metadata. Subsequent requests must fix snapshot_seq to the original; records with seq above it are excluded. after_seq > snapshot_seq, negative values, limits outside 1..500 or snapshot_seq above current max are invalid_cursor. No offsets and no opaque cursor state. Summary on the first page is computed at snapshot_seq, not from a newer mutable cache. Consumers check continuity and full pagination before making a decision. Empty arrays are [], never null.

GET content for verdict/completed/joined/import returns base64 of immutable exact bytes plus digest and size; other kinds return not_found. The post response contains the server-normalized record and its summary. No evidence bytes appear in record list or receipt; content is fetched explicitly. The exact UTF-8 verdict grammar below is normative. CRLF/LF bytes are preserved, not canonicalized for hashing.

The following JSON is a JSON Schema 2020-12 contract bundle. It defines closed request/output schemas, endpoint bindings and exact status/code enumerations. refs resolve within this bundle. Rules in §4 are additional normative state-transition contracts; implementers must run the vectors following the bundle against their own implementation. Nullable fields shown are REQUIRED even when null; fields not listed are rejected. content_b64 uses RFC4648 standard padded encoding; decoding must be canonical (re-encode equals input).

~~~json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$id": "urn:handoffkeep:hk1873:v1",
  "contract": "hk1873.v1",
  "request_max_bytes": 1048576,
  "decoded_content_max_bytes": 262144,
  "endpoints": [
    {
      "method": "GET",
      "path": "/v1/wrk/capabilities",
      "request": null,
      "success": {
        "200": {
          "$ref": "#/$defs/Capabilities"
        }
      }
    },
    {
      "method": "POST",
      "path": "/v1/wrk/jobs/{job_id}/records",
      "headers": {
        "Idempotency-Key": {
          "type": "string",
          "format": "uuid",
          "pattern": "^[0-9a-f-]{36}$"
        },
        "Content-Type": {
          "const": "application/json"
        }
      },
      "request": {
        "$ref": "#/$defs/Request"
      },
      "success": {
        "201": {
          "$ref": "#/$defs/WriteResponse"
        },
        "200": {
          "$ref": "#/$defs/WriteResponse"
        }
      }
    },
    {
      "method": "GET",
      "path": "/v1/wrk/jobs/{job_id}",
      "request": null,
      "success": {
        "200": {
          "$ref": "#/$defs/JobResponse"
        }
      }
    },
    {
      "method": "GET",
      "path": "/v1/wrk/jobs/{job_id}/records",
      "query": {
        "after_seq": {
          "type": "integer",
          "minimum": 0,
          "maximum": 9007199254740991,
          "default": 0
        },
        "snapshot_seq": {
          "type": "integer",
          "minimum": 1,
          "maximum": 9007199254740991
        },
        "limit": {
          "type": "integer",
          "minimum": 1,
          "maximum": 500,
          "default": 100
        }
      },
      "request": null,
      "success": {
        "200": {
          "$ref": "#/$defs/RecordsResponse"
        }
      }
    },
    {
      "method": "GET",
      "path": "/v1/wrk/jobs/{job_id}/records/{seq}/content",
      "request": null,
      "success": {
        "200": {
          "$ref": "#/$defs/ContentResponse"
        }
      }
    }
  ],
  "errors_by_status": {
    "400": [
      "invalid_request",
      "invalid_cursor",
      "invalid_verdict",
      "invalid_path",
      "grant_before_cap"
    ],
    "401": [
      "unauthorized"
    ],
    "403": [
      "scope_denied",
      "host_mismatch"
    ],
    "404": [
      "not_found"
    ],
    "409": [
      "idempotency_conflict",
      "sequence_conflict",
      "claim_conflict",
      "job_ended",
      "review_sealed",
      "round_cap",
      "round_not_open",
      "round_has_verdict",
      "round_voided",
      "head_mismatch",
      "duplicate_path",
      "duplicate_content",
      "duplicate_head",
      "grant_invalid",
      "grant_consumed",
      "grant_expired",
      "zero_rounds",
      "review_incomplete",
      "blocker_unresolved",
      "wrk_receipt_required",
      "import_unsealed",
      "import_sealed",
      "migration_frozen"
    ],
    "413": [
      "request_too_large"
    ],
    "415": [
      "unsupported_media_type"
    ],
    "503": [
      "wrk_unavailable",
      "wrk_recovery_required"
    ],
    "405": [
      "method_not_allowed"
    ]
  },
  "$defs": {
    "Request": {
      "oneOf": [
        {
          "type": "object",
          "properties": {
            "kind": {
              "const": "claim"
            },
            "claim_seq": {
              "type": "integer",
              "minimum": 0,
              "maximum": 9007199254740991
            },
            "expected_seq": {
              "type": "integer",
              "minimum": 0,
              "maximum": 9007199254740991
            },
            "payload": {
              "$ref": "#/$defs/Input_claim"
            }
          },
          "required": [
            "kind",
            "claim_seq",
            "expected_seq",
            "payload"
          ],
          "additionalProperties": false
        },
        {
          "type": "object",
          "properties": {
            "kind": {
              "const": "activate"
            },
            "claim_seq": {
              "type": "integer",
              "minimum": 0,
              "maximum": 9007199254740991
            },
            "expected_seq": {
              "type": "integer",
              "minimum": 0,
              "maximum": 9007199254740991
            },
            "payload": {
              "$ref": "#/$defs/Input_activate"
            }
          },
          "required": [
            "kind",
            "claim_seq",
            "expected_seq",
            "payload"
          ],
          "additionalProperties": false
        },
        {
          "type": "object",
          "properties": {
            "kind": {
              "const": "round_open"
            },
            "claim_seq": {
              "type": "integer",
              "minimum": 0,
              "maximum": 9007199254740991
            },
            "expected_seq": {
              "type": "integer",
              "minimum": 0,
              "maximum": 9007199254740991
            },
            "payload": {
              "$ref": "#/$defs/Input_round_open"
            }
          },
          "required": [
            "kind",
            "claim_seq",
            "expected_seq",
            "payload"
          ],
          "additionalProperties": false
        },
        {
          "type": "object",
          "properties": {
            "kind": {
              "const": "verdict"
            },
            "claim_seq": {
              "type": "integer",
              "minimum": 0,
              "maximum": 9007199254740991
            },
            "expected_seq": {
              "type": "integer",
              "minimum": 0,
              "maximum": 9007199254740991
            },
            "payload": {
              "$ref": "#/$defs/Input_verdict"
            }
          },
          "required": [
            "kind",
            "claim_seq",
            "expected_seq",
            "payload"
          ],
          "additionalProperties": false
        },
        {
          "type": "object",
          "properties": {
            "kind": {
              "const": "void"
            },
            "claim_seq": {
              "type": "integer",
              "minimum": 0,
              "maximum": 9007199254740991
            },
            "expected_seq": {
              "type": "integer",
              "minimum": 0,
              "maximum": 9007199254740991
            },
            "payload": {
              "$ref": "#/$defs/Input_void"
            }
          },
          "required": [
            "kind",
            "claim_seq",
            "expected_seq",
            "payload"
          ],
          "additionalProperties": false
        },
        {
          "type": "object",
          "properties": {
            "kind": {
              "const": "joined"
            },
            "claim_seq": {
              "type": "integer",
              "minimum": 0,
              "maximum": 9007199254740991
            },
            "expected_seq": {
              "type": "integer",
              "minimum": 0,
              "maximum": 9007199254740991
            },
            "payload": {
              "$ref": "#/$defs/Input_joined"
            }
          },
          "required": [
            "kind",
            "claim_seq",
            "expected_seq",
            "payload"
          ],
          "additionalProperties": false
        },
        {
          "type": "object",
          "properties": {
            "kind": {
              "const": "completed"
            },
            "claim_seq": {
              "type": "integer",
              "minimum": 0,
              "maximum": 9007199254740991
            },
            "expected_seq": {
              "type": "integer",
              "minimum": 0,
              "maximum": 9007199254740991
            },
            "payload": {
              "$ref": "#/$defs/Input_completed"
            }
          },
          "required": [
            "kind",
            "claim_seq",
            "expected_seq",
            "payload"
          ],
          "additionalProperties": false
        },
        {
          "type": "object",
          "properties": {
            "kind": {
              "const": "grant"
            },
            "claim_seq": {
              "type": "integer",
              "minimum": 0,
              "maximum": 9007199254740991
            },
            "expected_seq": {
              "type": "integer",
              "minimum": 0,
              "maximum": 9007199254740991
            },
            "payload": {
              "$ref": "#/$defs/Input_grant"
            }
          },
          "required": [
            "kind",
            "claim_seq",
            "expected_seq",
            "payload"
          ],
          "additionalProperties": false
        },
        {
          "type": "object",
          "properties": {
            "kind": {
              "const": "grant_cancel"
            },
            "claim_seq": {
              "type": "integer",
              "minimum": 0,
              "maximum": 9007199254740991
            },
            "expected_seq": {
              "type": "integer",
              "minimum": 0,
              "maximum": 9007199254740991
            },
            "payload": {
              "$ref": "#/$defs/Input_grant_cancel"
            }
          },
          "required": [
            "kind",
            "claim_seq",
            "expected_seq",
            "payload"
          ],
          "additionalProperties": false
        },
        {
          "type": "object",
          "properties": {
            "kind": {
              "const": "end"
            },
            "claim_seq": {
              "type": "integer",
              "minimum": 0,
              "maximum": 9007199254740991
            },
            "expected_seq": {
              "type": "integer",
              "minimum": 0,
              "maximum": 9007199254740991
            },
            "payload": {
              "$ref": "#/$defs/Input_end"
            }
          },
          "required": [
            "kind",
            "claim_seq",
            "expected_seq",
            "payload"
          ],
          "additionalProperties": false
        },
        {
          "type": "object",
          "properties": {
            "kind": {
              "const": "import"
            },
            "claim_seq": {
              "type": "integer",
              "minimum": 0,
              "maximum": 9007199254740991
            },
            "expected_seq": {
              "type": "integer",
              "minimum": 0,
              "maximum": 9007199254740991
            },
            "payload": {
              "$ref": "#/$defs/Input_import"
            }
          },
          "required": [
            "kind",
            "claim_seq",
            "expected_seq",
            "payload"
          ],
          "additionalProperties": false
        },
        {
          "type": "object",
          "properties": {
            "kind": {
              "const": "import_begin"
            },
            "claim_seq": {
              "type": "integer",
              "minimum": 0,
              "maximum": 9007199254740991
            },
            "expected_seq": {
              "type": "integer",
              "minimum": 0,
              "maximum": 9007199254740991
            },
            "payload": {
              "$ref": "#/$defs/Input_import_begin"
            }
          },
          "required": [
            "kind",
            "claim_seq",
            "expected_seq",
            "payload"
          ],
          "additionalProperties": false
        },
        {
          "type": "object",
          "properties": {
            "kind": {
              "const": "import_seal"
            },
            "claim_seq": {
              "type": "integer",
              "minimum": 0,
              "maximum": 9007199254740991
            },
            "expected_seq": {
              "type": "integer",
              "minimum": 0,
              "maximum": 9007199254740991
            },
            "payload": {
              "$ref": "#/$defs/Input_import_seal"
            }
          },
          "required": [
            "kind",
            "claim_seq",
            "expected_seq",
            "payload"
          ],
          "additionalProperties": false
        }
      ]
    },
    "Input_claim": {
      "type": "object",
      "properties": {
        "job_id": {
          "type": "string",
          "pattern": "^[a-z0-9][a-z0-9._-]{0,127}$"
        },
        "owner_lane": {
          "type": "string",
          "pattern": "^[a-z0-9][a-z0-9-]{0,63}$"
        },
        "parent_lane": {
          "type": "string",
          "pattern": "^[a-z0-9][a-z0-9-]{0,63}$"
        },
        "task_id": {
          "type": "integer",
          "minimum": 1,
          "maximum": 9007199254740991
        },
        "host_client_id": {
          "enum": [
            "desktop",
            "mac-dev",
            "mac-work",
            "rpi",
            "mac-personal"
          ]
        },
        "role": {
          "enum": [
            "builder",
            "worker",
            "tester",
            "reviewer",
            "counsel"
          ]
        },
        "tier": {
          "enum": [
            "T0",
            "T1",
            "T2",
            "T3"
          ]
        },
        "review_of": {
          "anyOf": [
            {
              "type": "object",
              "properties": {
                "job_id": {
                  "type": "string",
                  "pattern": "^[a-z0-9][a-z0-9._-]{0,127}$"
                },
                "round_no": {
                  "type": "integer",
                  "minimum": 1,
                  "maximum": 9007199254740991
                }
              },
              "required": [
                "job_id",
                "round_no"
              ],
              "additionalProperties": false
            },
            {
              "type": "null"
            }
          ]
        },
        "predecessor_job_id": {
          "anyOf": [
            {
              "type": "string",
              "pattern": "^[a-z0-9][a-z0-9._-]{0,127}$"
            },
            {
              "type": "null"
            }
          ]
        },
        "supersedes": {
          "anyOf": [
            {
              "type": "integer",
              "minimum": 1,
              "maximum": 9007199254740991
            },
            {
              "type": "null"
            }
          ]
        }
      },
      "required": [
        "job_id",
        "owner_lane",
        "parent_lane",
        "task_id",
        "host_client_id",
        "role",
        "tier",
        "review_of",
        "predecessor_job_id",
        "supersedes"
      ],
      "additionalProperties": false
    },
    "Payload_claim": {
      "type": "object",
      "properties": {
        "job_id": {
          "type": "string",
          "pattern": "^[a-z0-9][a-z0-9._-]{0,127}$"
        },
        "owner_lane": {
          "type": "string",
          "pattern": "^[a-z0-9][a-z0-9-]{0,63}$"
        },
        "parent_lane": {
          "type": "string",
          "pattern": "^[a-z0-9][a-z0-9-]{0,63}$"
        },
        "task_id": {
          "type": "integer",
          "minimum": 1,
          "maximum": 9007199254740991
        },
        "host_client_id": {
          "enum": [
            "desktop",
            "mac-dev",
            "mac-work",
            "rpi",
            "mac-personal"
          ]
        },
        "role": {
          "enum": [
            "builder",
            "worker",
            "tester",
            "reviewer",
            "counsel"
          ]
        },
        "tier": {
          "enum": [
            "T0",
            "T1",
            "T2",
            "T3"
          ]
        },
        "review_of": {
          "anyOf": [
            {
              "type": "object",
              "properties": {
                "job_id": {
                  "type": "string",
                  "pattern": "^[a-z0-9][a-z0-9._-]{0,127}$"
                },
                "round_no": {
                  "type": "integer",
                  "minimum": 1,
                  "maximum": 9007199254740991
                }
              },
              "required": [
                "job_id",
                "round_no"
              ],
              "additionalProperties": false
            },
            {
              "type": "null"
            }
          ]
        },
        "predecessor_job_id": {
          "anyOf": [
            {
              "type": "string",
              "pattern": "^[a-z0-9][a-z0-9._-]{0,127}$"
            },
            {
              "type": "null"
            }
          ]
        },
        "supersedes": {
          "anyOf": [
            {
              "type": "integer",
              "minimum": 1,
              "maximum": 9007199254740991
            },
            {
              "type": "null"
            }
          ]
        },
        "policy_sha256": {
          "type": "string",
          "pattern": "^[0-9a-f]{64}$"
        },
        "created_at": {
          "type": "string",
          "format": "date-time"
        }
      },
      "required": [
        "job_id",
        "owner_lane",
        "parent_lane",
        "task_id",
        "host_client_id",
        "role",
        "tier",
        "review_of",
        "predecessor_job_id",
        "supersedes",
        "policy_sha256",
        "created_at"
      ],
      "additionalProperties": false
    },
    "Input_activate": {
      "type": "object",
      "properties": {
        "reason_code": {
          "type": "string",
          "pattern": "^[a-z][a-z0-9_]{0,63}$"
        },
        "seal_seq": {
          "anyOf": [
            {
              "type": "integer",
              "minimum": 1,
              "maximum": 9007199254740991
            },
            {
              "type": "null"
            }
          ]
        }
      },
      "required": [
        "reason_code",
        "seal_seq"
      ],
      "additionalProperties": false
    },
    "Payload_activate": {
      "type": "object",
      "properties": {
        "baseline_used": {
          "type": "integer",
          "minimum": 0,
          "maximum": 9007199254740991
        },
        "mode": {
          "enum": [
            "enforced"
          ]
        },
        "reason_code": {
          "type": "string",
          "pattern": "^[a-z][a-z0-9_]{0,63}$"
        },
        "seal_seq": {
          "anyOf": [
            {
              "type": "integer",
              "minimum": 1,
              "maximum": 9007199254740991
            },
            {
              "type": "null"
            }
          ]
        }
      },
      "required": [
        "baseline_used",
        "mode",
        "reason_code",
        "seal_seq"
      ],
      "additionalProperties": false
    },
    "Input_round_open": {
      "type": "object",
      "properties": {
        "head": {
          "type": "string",
          "pattern": "^[0-9a-f]{40}$"
        },
        "tester_job_id": {
          "anyOf": [
            {
              "type": "string",
              "pattern": "^[a-z0-9][a-z0-9._-]{0,127}$"
            },
            {
              "type": "null"
            }
          ]
        },
        "extension_grant_seq": {
          "anyOf": [
            {
              "type": "integer",
              "minimum": 1,
              "maximum": 9007199254740991
            },
            {
              "type": "null"
            }
          ]
        },
        "repeat_grant_seq": {
          "anyOf": [
            {
              "type": "integer",
              "minimum": 1,
              "maximum": 9007199254740991
            },
            {
              "type": "null"
            }
          ]
        }
      },
      "required": [
        "head",
        "tester_job_id",
        "extension_grant_seq",
        "repeat_grant_seq"
      ],
      "additionalProperties": false
    },
    "Payload_round_open": {
      "type": "object",
      "properties": {
        "round_no": {
          "type": "integer",
          "minimum": 1,
          "maximum": 9007199254740991
        },
        "head": {
          "type": "string",
          "pattern": "^[0-9a-f]{40}$"
        },
        "tester_job_id": {
          "anyOf": [
            {
              "type": "string",
              "pattern": "^[a-z0-9][a-z0-9._-]{0,127}$"
            },
            {
              "type": "null"
            }
          ]
        },
        "extension_grant_seq": {
          "anyOf": [
            {
              "type": "integer",
              "minimum": 1,
              "maximum": 9007199254740991
            },
            {
              "type": "null"
            }
          ]
        },
        "repeat_grant_seq": {
          "anyOf": [
            {
              "type": "integer",
              "minimum": 1,
              "maximum": 9007199254740991
            },
            {
              "type": "null"
            }
          ]
        }
      },
      "required": [
        "round_no",
        "head",
        "tester_job_id",
        "extension_grant_seq",
        "repeat_grant_seq"
      ],
      "additionalProperties": false
    },
    "Input_verdict": {
      "type": "object",
      "properties": {
        "round_no": {
          "type": "integer",
          "minimum": 1,
          "maximum": 9007199254740991
        },
        "head": {
          "type": "string",
          "pattern": "^[0-9a-f]{40}$"
        },
        "verdict": {
          "enum": [
            "PASS",
            "BLOCKER"
          ]
        },
        "path": {
          "type": "string",
          "minLength": 1,
          "maxLength": 4096
        },
        "content_sha256": {
          "type": "string",
          "pattern": "^[0-9a-f]{64}$"
        },
        "content_b64": {
          "type": "string",
          "contentEncoding": "base64",
          "minLength": 4,
          "maxLength": 349528
        },
        "tester_job_id": {
          "anyOf": [
            {
              "type": "string",
              "pattern": "^[a-z0-9][a-z0-9._-]{0,127}$"
            },
            {
              "type": "null"
            }
          ]
        }
      },
      "required": [
        "round_no",
        "head",
        "verdict",
        "path",
        "content_sha256",
        "content_b64",
        "tester_job_id"
      ],
      "additionalProperties": false
    },
    "Payload_verdict": {
      "type": "object",
      "properties": {
        "round_no": {
          "type": "integer",
          "minimum": 1,
          "maximum": 9007199254740991
        },
        "head": {
          "type": "string",
          "pattern": "^[0-9a-f]{40}$"
        },
        "verdict": {
          "enum": [
            "PASS",
            "BLOCKER"
          ]
        },
        "path": {
          "type": "string",
          "minLength": 1,
          "maxLength": 4096
        },
        "content_sha256": {
          "type": "string",
          "pattern": "^[0-9a-f]{64}$"
        },
        "tester_job_id": {
          "anyOf": [
            {
              "type": "string",
              "pattern": "^[a-z0-9][a-z0-9._-]{0,127}$"
            },
            {
              "type": "null"
            }
          ]
        },
        "size_bytes": {
          "type": "integer",
          "minimum": 1,
          "maximum": 9007199254740991
        }
      },
      "required": [
        "round_no",
        "head",
        "verdict",
        "path",
        "content_sha256",
        "tester_job_id",
        "size_bytes"
      ],
      "additionalProperties": false
    },
    "Input_void": {
      "type": "object",
      "properties": {
        "round_no": {
          "type": "integer",
          "minimum": 1,
          "maximum": 9007199254740991
        },
        "grant_seq": {
          "type": "integer",
          "minimum": 1,
          "maximum": 9007199254740991
        },
        "reason_code": {
          "type": "string",
          "pattern": "^[a-z][a-z0-9_]{0,63}$"
        }
      },
      "required": [
        "round_no",
        "grant_seq",
        "reason_code"
      ],
      "additionalProperties": false
    },
    "Payload_void": {
      "type": "object",
      "properties": {
        "round_no": {
          "type": "integer",
          "minimum": 1,
          "maximum": 9007199254740991
        },
        "grant_seq": {
          "type": "integer",
          "minimum": 1,
          "maximum": 9007199254740991
        },
        "reason_code": {
          "type": "string",
          "pattern": "^[a-z][a-z0-9_]{0,63}$"
        }
      },
      "required": [
        "round_no",
        "grant_seq",
        "reason_code"
      ],
      "additionalProperties": false
    },
    "Input_joined": {
      "type": "object",
      "properties": {
        "head": {
          "type": "string",
          "pattern": "^[0-9a-f]{40}$"
        },
        "pr": {
          "type": "string",
          "format": "uri",
          "maxLength": 4096
        },
        "report_b64": {
          "type": "string",
          "contentEncoding": "base64",
          "minLength": 4,
          "maxLength": 349528
        },
        "report_sha256": {
          "type": "string",
          "pattern": "^[0-9a-f]{64}$"
        },
        "waiver_grant_seq": {
          "anyOf": [
            {
              "type": "integer",
              "minimum": 1,
              "maximum": 9007199254740991
            },
            {
              "type": "null"
            }
          ]
        }
      },
      "required": [
        "head",
        "pr",
        "report_b64",
        "report_sha256",
        "waiver_grant_seq"
      ],
      "additionalProperties": false
    },
    "Payload_joined": {
      "type": "object",
      "properties": {
        "head": {
          "type": "string",
          "pattern": "^[0-9a-f]{40}$"
        },
        "pr": {
          "type": "string",
          "format": "uri",
          "maxLength": 4096
        },
        "report_sha256": {
          "type": "string",
          "pattern": "^[0-9a-f]{64}$"
        },
        "report_size_bytes": {
          "type": "integer",
          "minimum": 1,
          "maximum": 9007199254740991
        },
        "verdict_seqs": {
          "type": "array",
          "items": {
            "type": "integer",
            "minimum": 1,
            "maximum": 9007199254740991
          }
        },
        "waiver_grant_seq": {
          "anyOf": [
            {
              "type": "integer",
              "minimum": 1,
              "maximum": 9007199254740991
            },
            {
              "type": "null"
            }
          ]
        }
      },
      "required": [
        "head",
        "pr",
        "report_sha256",
        "report_size_bytes",
        "verdict_seqs",
        "waiver_grant_seq"
      ],
      "additionalProperties": false
    },
    "Input_completed": {
      "type": "object",
      "properties": {
        "report_b64": {
          "type": "string",
          "contentEncoding": "base64",
          "minLength": 4,
          "maxLength": 349528
        },
        "report_sha256": {
          "type": "string",
          "pattern": "^[0-9a-f]{64}$"
        }
      },
      "required": [
        "report_b64",
        "report_sha256"
      ],
      "additionalProperties": false
    },
    "Payload_completed": {
      "type": "object",
      "properties": {
        "report_sha256": {
          "type": "string",
          "pattern": "^[0-9a-f]{64}$"
        },
        "report_size_bytes": {
          "type": "integer",
          "minimum": 1,
          "maximum": 9007199254740991
        },
        "warnings": {
          "type": "array",
          "items": {
            "type": "string",
            "pattern": "^[a-z][a-z0-9_]{0,63}$"
          }
        }
      },
      "required": [
        "report_sha256",
        "report_size_bytes",
        "warnings"
      ],
      "additionalProperties": false
    },
    "Input_grant": {
      "type": "object",
      "properties": {
        "grant_type": {
          "enum": [
            "extension",
            "void",
            "head_repeat",
            "join_waiver"
          ]
        },
        "round_no": {
          "anyOf": [
            {
              "type": "integer",
              "minimum": 1,
              "maximum": 9007199254740991
            },
            {
              "type": "null"
            }
          ]
        },
        "head": {
          "anyOf": [
            {
              "type": "string",
              "pattern": "^[0-9a-f]{40}$"
            },
            {
              "type": "null"
            }
          ]
        },
        "extra": {
          "enum": [
            0,
            1
          ]
        },
        "earlier_round_no": {
          "anyOf": [
            {
              "type": "integer",
              "minimum": 1,
              "maximum": 9007199254740991
            },
            {
              "type": "null"
            }
          ]
        },
        "tester_job_id": {
          "anyOf": [
            {
              "type": "string",
              "pattern": "^[a-z0-9][a-z0-9._-]{0,127}$"
            },
            {
              "type": "null"
            }
          ]
        },
        "expires_at": {
          "type": "string",
          "format": "date-time"
        },
        "reason_code": {
          "type": "string",
          "pattern": "^[a-z][a-z0-9_]{0,63}$"
        }
      },
      "required": [
        "grant_type",
        "round_no",
        "head",
        "extra",
        "earlier_round_no",
        "tester_job_id",
        "expires_at",
        "reason_code"
      ],
      "additionalProperties": false
    },
    "Payload_grant": {
      "type": "object",
      "properties": {
        "grant_type": {
          "enum": [
            "extension",
            "void",
            "head_repeat",
            "join_waiver"
          ]
        },
        "round_no": {
          "anyOf": [
            {
              "type": "integer",
              "minimum": 1,
              "maximum": 9007199254740991
            },
            {
              "type": "null"
            }
          ]
        },
        "head": {
          "anyOf": [
            {
              "type": "string",
              "pattern": "^[0-9a-f]{40}$"
            },
            {
              "type": "null"
            }
          ]
        },
        "extra": {
          "enum": [
            0,
            1
          ]
        },
        "earlier_round_no": {
          "anyOf": [
            {
              "type": "integer",
              "minimum": 1,
              "maximum": 9007199254740991
            },
            {
              "type": "null"
            }
          ]
        },
        "tester_job_id": {
          "anyOf": [
            {
              "type": "string",
              "pattern": "^[a-z0-9][a-z0-9._-]{0,127}$"
            },
            {
              "type": "null"
            }
          ]
        },
        "expires_at": {
          "type": "string",
          "format": "date-time"
        },
        "reason_code": {
          "type": "string",
          "pattern": "^[a-z][a-z0-9_]{0,63}$"
        }
      },
      "required": [
        "grant_type",
        "round_no",
        "head",
        "extra",
        "earlier_round_no",
        "tester_job_id",
        "expires_at",
        "reason_code"
      ],
      "additionalProperties": false
    },
    "Input_grant_cancel": {
      "type": "object",
      "properties": {
        "grant_seq": {
          "type": "integer",
          "minimum": 1,
          "maximum": 9007199254740991
        },
        "reason_code": {
          "type": "string",
          "pattern": "^[a-z][a-z0-9_]{0,63}$"
        }
      },
      "required": [
        "grant_seq",
        "reason_code"
      ],
      "additionalProperties": false
    },
    "Payload_grant_cancel": {
      "type": "object",
      "properties": {
        "grant_seq": {
          "type": "integer",
          "minimum": 1,
          "maximum": 9007199254740991
        },
        "reason_code": {
          "type": "string",
          "pattern": "^[a-z][a-z0-9_]{0,63}$"
        }
      },
      "required": [
        "grant_seq",
        "reason_code"
      ],
      "additionalProperties": false
    },
    "Input_end": {
      "type": "object",
      "properties": {
        "end_type": {
          "enum": [
            "revoked",
            "reaped",
            "handed_off"
          ]
        },
        "reason_code": {
          "type": "string",
          "pattern": "^[a-z][a-z0-9_]{0,63}$"
        }
      },
      "required": [
        "end_type",
        "reason_code"
      ],
      "additionalProperties": false
    },
    "Payload_end": {
      "type": "object",
      "properties": {
        "end_type": {
          "enum": [
            "revoked",
            "reaped",
            "handed_off"
          ]
        },
        "reason_code": {
          "type": "string",
          "pattern": "^[a-z][a-z0-9_]{0,63}$"
        }
      },
      "required": [
        "end_type",
        "reason_code"
      ],
      "additionalProperties": false
    },
    "Input_import": {
      "type": "object",
      "properties": {
        "source_locator": {
          "type": "string",
          "minLength": 1,
          "maxLength": 4096
        },
        "source_sha256": {
          "type": "string",
          "pattern": "^[0-9a-f]{64}$"
        },
        "original_actor_client_id": {
          "anyOf": [
            {
              "type": "string",
              "pattern": "^[a-z0-9][a-z0-9._-]{0,127}$"
            },
            {
              "type": "null"
            }
          ]
        },
        "original_at": {
          "anyOf": [
            {
              "type": "string",
              "format": "date-time"
            },
            {
              "type": "null"
            }
          ]
        },
        "legacy_kind": {
          "type": "string",
          "minLength": 1,
          "maxLength": 64
        },
        "content_b64": {
          "type": "string",
          "contentEncoding": "base64",
          "minLength": 4,
          "maxLength": 349528
        }
      },
      "required": [
        "source_locator",
        "source_sha256",
        "original_actor_client_id",
        "original_at",
        "legacy_kind",
        "content_b64"
      ],
      "additionalProperties": false
    },
    "Payload_import": {
      "type": "object",
      "properties": {
        "source_locator": {
          "type": "string",
          "minLength": 1,
          "maxLength": 4096
        },
        "source_sha256": {
          "type": "string",
          "pattern": "^[0-9a-f]{64}$"
        },
        "original_actor_client_id": {
          "anyOf": [
            {
              "type": "string",
              "pattern": "^[a-z0-9][a-z0-9._-]{0,127}$"
            },
            {
              "type": "null"
            }
          ]
        },
        "original_at": {
          "anyOf": [
            {
              "type": "string",
              "format": "date-time"
            },
            {
              "type": "null"
            }
          ]
        },
        "legacy_kind": {
          "type": "string",
          "minLength": 1,
          "maxLength": 64
        }
      },
      "required": [
        "source_locator",
        "source_sha256",
        "original_actor_client_id",
        "original_at",
        "legacy_kind"
      ],
      "additionalProperties": false
    },
    "Record": {
      "oneOf": [
        {
          "type": "object",
          "properties": {
            "job_id": {
              "type": "string",
              "pattern": "^[a-z0-9][a-z0-9._-]{0,127}$"
            },
            "seq": {
              "type": "integer",
              "minimum": 1,
              "maximum": 9007199254740991
            },
            "record_id": {
              "type": "string",
              "format": "uuid",
              "pattern": "^[0-9a-f-]{36}$"
            },
            "store_generation": {
              "type": "string",
              "format": "uuid",
              "pattern": "^[0-9a-f-]{36}$"
            },
            "claim_seq": {
              "type": "integer",
              "minimum": 0,
              "maximum": 9007199254740991
            },
            "actor_client_id": {
              "type": "string",
              "pattern": "^[a-z0-9][a-z0-9._-]{0,127}$"
            },
            "idempotency_key": {
              "type": "string",
              "format": "uuid",
              "pattern": "^[0-9a-f-]{36}$"
            },
            "request_sha256": {
              "type": "string",
              "pattern": "^[0-9a-f]{64}$"
            },
            "imported": {
              "type": "boolean"
            },
            "source": {
              "anyOf": [
                {
                  "type": "object",
                  "properties": {
                    "source_locator": {
                      "type": "string",
                      "maxLength": 4096
                    },
                    "source_sha256": {
                      "type": "string",
                      "pattern": "^[0-9a-f]{64}$"
                    }
                  },
                  "required": [
                    "source_locator",
                    "source_sha256"
                  ],
                  "additionalProperties": false
                },
                {
                  "type": "null"
                }
              ]
            },
            "received_at": {
              "type": "string",
              "format": "date-time"
            },
            "previous_hash": {
              "type": "string",
              "pattern": "^[0-9a-f]{64}$"
            },
            "record_hash": {
              "type": "string",
              "pattern": "^[0-9a-f]{64}$"
            },
            "kind": {
              "const": "claim"
            },
            "payload": {
              "$ref": "#/$defs/Payload_claim"
            }
          },
          "required": [
            "job_id",
            "seq",
            "record_id",
            "store_generation",
            "claim_seq",
            "actor_client_id",
            "idempotency_key",
            "request_sha256",
            "imported",
            "source",
            "received_at",
            "previous_hash",
            "record_hash",
            "kind",
            "payload"
          ],
          "additionalProperties": false
        },
        {
          "type": "object",
          "properties": {
            "job_id": {
              "type": "string",
              "pattern": "^[a-z0-9][a-z0-9._-]{0,127}$"
            },
            "seq": {
              "type": "integer",
              "minimum": 1,
              "maximum": 9007199254740991
            },
            "record_id": {
              "type": "string",
              "format": "uuid",
              "pattern": "^[0-9a-f-]{36}$"
            },
            "store_generation": {
              "type": "string",
              "format": "uuid",
              "pattern": "^[0-9a-f-]{36}$"
            },
            "claim_seq": {
              "type": "integer",
              "minimum": 0,
              "maximum": 9007199254740991
            },
            "actor_client_id": {
              "type": "string",
              "pattern": "^[a-z0-9][a-z0-9._-]{0,127}$"
            },
            "idempotency_key": {
              "type": "string",
              "format": "uuid",
              "pattern": "^[0-9a-f-]{36}$"
            },
            "request_sha256": {
              "type": "string",
              "pattern": "^[0-9a-f]{64}$"
            },
            "imported": {
              "type": "boolean"
            },
            "source": {
              "anyOf": [
                {
                  "type": "object",
                  "properties": {
                    "source_locator": {
                      "type": "string",
                      "maxLength": 4096
                    },
                    "source_sha256": {
                      "type": "string",
                      "pattern": "^[0-9a-f]{64}$"
                    }
                  },
                  "required": [
                    "source_locator",
                    "source_sha256"
                  ],
                  "additionalProperties": false
                },
                {
                  "type": "null"
                }
              ]
            },
            "received_at": {
              "type": "string",
              "format": "date-time"
            },
            "previous_hash": {
              "type": "string",
              "pattern": "^[0-9a-f]{64}$"
            },
            "record_hash": {
              "type": "string",
              "pattern": "^[0-9a-f]{64}$"
            },
            "kind": {
              "const": "activate"
            },
            "payload": {
              "$ref": "#/$defs/Payload_activate"
            }
          },
          "required": [
            "job_id",
            "seq",
            "record_id",
            "store_generation",
            "claim_seq",
            "actor_client_id",
            "idempotency_key",
            "request_sha256",
            "imported",
            "source",
            "received_at",
            "previous_hash",
            "record_hash",
            "kind",
            "payload"
          ],
          "additionalProperties": false
        },
        {
          "type": "object",
          "properties": {
            "job_id": {
              "type": "string",
              "pattern": "^[a-z0-9][a-z0-9._-]{0,127}$"
            },
            "seq": {
              "type": "integer",
              "minimum": 1,
              "maximum": 9007199254740991
            },
            "record_id": {
              "type": "string",
              "format": "uuid",
              "pattern": "^[0-9a-f-]{36}$"
            },
            "store_generation": {
              "type": "string",
              "format": "uuid",
              "pattern": "^[0-9a-f-]{36}$"
            },
            "claim_seq": {
              "type": "integer",
              "minimum": 0,
              "maximum": 9007199254740991
            },
            "actor_client_id": {
              "type": "string",
              "pattern": "^[a-z0-9][a-z0-9._-]{0,127}$"
            },
            "idempotency_key": {
              "type": "string",
              "format": "uuid",
              "pattern": "^[0-9a-f-]{36}$"
            },
            "request_sha256": {
              "type": "string",
              "pattern": "^[0-9a-f]{64}$"
            },
            "imported": {
              "type": "boolean"
            },
            "source": {
              "anyOf": [
                {
                  "type": "object",
                  "properties": {
                    "source_locator": {
                      "type": "string",
                      "maxLength": 4096
                    },
                    "source_sha256": {
                      "type": "string",
                      "pattern": "^[0-9a-f]{64}$"
                    }
                  },
                  "required": [
                    "source_locator",
                    "source_sha256"
                  ],
                  "additionalProperties": false
                },
                {
                  "type": "null"
                }
              ]
            },
            "received_at": {
              "type": "string",
              "format": "date-time"
            },
            "previous_hash": {
              "type": "string",
              "pattern": "^[0-9a-f]{64}$"
            },
            "record_hash": {
              "type": "string",
              "pattern": "^[0-9a-f]{64}$"
            },
            "kind": {
              "const": "round_open"
            },
            "payload": {
              "$ref": "#/$defs/Payload_round_open"
            }
          },
          "required": [
            "job_id",
            "seq",
            "record_id",
            "store_generation",
            "claim_seq",
            "actor_client_id",
            "idempotency_key",
            "request_sha256",
            "imported",
            "source",
            "received_at",
            "previous_hash",
            "record_hash",
            "kind",
            "payload"
          ],
          "additionalProperties": false
        },
        {
          "type": "object",
          "properties": {
            "job_id": {
              "type": "string",
              "pattern": "^[a-z0-9][a-z0-9._-]{0,127}$"
            },
            "seq": {
              "type": "integer",
              "minimum": 1,
              "maximum": 9007199254740991
            },
            "record_id": {
              "type": "string",
              "format": "uuid",
              "pattern": "^[0-9a-f-]{36}$"
            },
            "store_generation": {
              "type": "string",
              "format": "uuid",
              "pattern": "^[0-9a-f-]{36}$"
            },
            "claim_seq": {
              "type": "integer",
              "minimum": 0,
              "maximum": 9007199254740991
            },
            "actor_client_id": {
              "type": "string",
              "pattern": "^[a-z0-9][a-z0-9._-]{0,127}$"
            },
            "idempotency_key": {
              "type": "string",
              "format": "uuid",
              "pattern": "^[0-9a-f-]{36}$"
            },
            "request_sha256": {
              "type": "string",
              "pattern": "^[0-9a-f]{64}$"
            },
            "imported": {
              "type": "boolean"
            },
            "source": {
              "anyOf": [
                {
                  "type": "object",
                  "properties": {
                    "source_locator": {
                      "type": "string",
                      "maxLength": 4096
                    },
                    "source_sha256": {
                      "type": "string",
                      "pattern": "^[0-9a-f]{64}$"
                    }
                  },
                  "required": [
                    "source_locator",
                    "source_sha256"
                  ],
                  "additionalProperties": false
                },
                {
                  "type": "null"
                }
              ]
            },
            "received_at": {
              "type": "string",
              "format": "date-time"
            },
            "previous_hash": {
              "type": "string",
              "pattern": "^[0-9a-f]{64}$"
            },
            "record_hash": {
              "type": "string",
              "pattern": "^[0-9a-f]{64}$"
            },
            "kind": {
              "const": "verdict"
            },
            "payload": {
              "$ref": "#/$defs/Payload_verdict"
            }
          },
          "required": [
            "job_id",
            "seq",
            "record_id",
            "store_generation",
            "claim_seq",
            "actor_client_id",
            "idempotency_key",
            "request_sha256",
            "imported",
            "source",
            "received_at",
            "previous_hash",
            "record_hash",
            "kind",
            "payload"
          ],
          "additionalProperties": false
        },
        {
          "type": "object",
          "properties": {
            "job_id": {
              "type": "string",
              "pattern": "^[a-z0-9][a-z0-9._-]{0,127}$"
            },
            "seq": {
              "type": "integer",
              "minimum": 1,
              "maximum": 9007199254740991
            },
            "record_id": {
              "type": "string",
              "format": "uuid",
              "pattern": "^[0-9a-f-]{36}$"
            },
            "store_generation": {
              "type": "string",
              "format": "uuid",
              "pattern": "^[0-9a-f-]{36}$"
            },
            "claim_seq": {
              "type": "integer",
              "minimum": 0,
              "maximum": 9007199254740991
            },
            "actor_client_id": {
              "type": "string",
              "pattern": "^[a-z0-9][a-z0-9._-]{0,127}$"
            },
            "idempotency_key": {
              "type": "string",
              "format": "uuid",
              "pattern": "^[0-9a-f-]{36}$"
            },
            "request_sha256": {
              "type": "string",
              "pattern": "^[0-9a-f]{64}$"
            },
            "imported": {
              "type": "boolean"
            },
            "source": {
              "anyOf": [
                {
                  "type": "object",
                  "properties": {
                    "source_locator": {
                      "type": "string",
                      "maxLength": 4096
                    },
                    "source_sha256": {
                      "type": "string",
                      "pattern": "^[0-9a-f]{64}$"
                    }
                  },
                  "required": [
                    "source_locator",
                    "source_sha256"
                  ],
                  "additionalProperties": false
                },
                {
                  "type": "null"
                }
              ]
            },
            "received_at": {
              "type": "string",
              "format": "date-time"
            },
            "previous_hash": {
              "type": "string",
              "pattern": "^[0-9a-f]{64}$"
            },
            "record_hash": {
              "type": "string",
              "pattern": "^[0-9a-f]{64}$"
            },
            "kind": {
              "const": "void"
            },
            "payload": {
              "$ref": "#/$defs/Payload_void"
            }
          },
          "required": [
            "job_id",
            "seq",
            "record_id",
            "store_generation",
            "claim_seq",
            "actor_client_id",
            "idempotency_key",
            "request_sha256",
            "imported",
            "source",
            "received_at",
            "previous_hash",
            "record_hash",
            "kind",
            "payload"
          ],
          "additionalProperties": false
        },
        {
          "type": "object",
          "properties": {
            "job_id": {
              "type": "string",
              "pattern": "^[a-z0-9][a-z0-9._-]{0,127}$"
            },
            "seq": {
              "type": "integer",
              "minimum": 1,
              "maximum": 9007199254740991
            },
            "record_id": {
              "type": "string",
              "format": "uuid",
              "pattern": "^[0-9a-f-]{36}$"
            },
            "store_generation": {
              "type": "string",
              "format": "uuid",
              "pattern": "^[0-9a-f-]{36}$"
            },
            "claim_seq": {
              "type": "integer",
              "minimum": 0,
              "maximum": 9007199254740991
            },
            "actor_client_id": {
              "type": "string",
              "pattern": "^[a-z0-9][a-z0-9._-]{0,127}$"
            },
            "idempotency_key": {
              "type": "string",
              "format": "uuid",
              "pattern": "^[0-9a-f-]{36}$"
            },
            "request_sha256": {
              "type": "string",
              "pattern": "^[0-9a-f]{64}$"
            },
            "imported": {
              "type": "boolean"
            },
            "source": {
              "anyOf": [
                {
                  "type": "object",
                  "properties": {
                    "source_locator": {
                      "type": "string",
                      "maxLength": 4096
                    },
                    "source_sha256": {
                      "type": "string",
                      "pattern": "^[0-9a-f]{64}$"
                    }
                  },
                  "required": [
                    "source_locator",
                    "source_sha256"
                  ],
                  "additionalProperties": false
                },
                {
                  "type": "null"
                }
              ]
            },
            "received_at": {
              "type": "string",
              "format": "date-time"
            },
            "previous_hash": {
              "type": "string",
              "pattern": "^[0-9a-f]{64}$"
            },
            "record_hash": {
              "type": "string",
              "pattern": "^[0-9a-f]{64}$"
            },
            "kind": {
              "const": "joined"
            },
            "payload": {
              "$ref": "#/$defs/Payload_joined"
            }
          },
          "required": [
            "job_id",
            "seq",
            "record_id",
            "store_generation",
            "claim_seq",
            "actor_client_id",
            "idempotency_key",
            "request_sha256",
            "imported",
            "source",
            "received_at",
            "previous_hash",
            "record_hash",
            "kind",
            "payload"
          ],
          "additionalProperties": false
        },
        {
          "type": "object",
          "properties": {
            "job_id": {
              "type": "string",
              "pattern": "^[a-z0-9][a-z0-9._-]{0,127}$"
            },
            "seq": {
              "type": "integer",
              "minimum": 1,
              "maximum": 9007199254740991
            },
            "record_id": {
              "type": "string",
              "format": "uuid",
              "pattern": "^[0-9a-f-]{36}$"
            },
            "store_generation": {
              "type": "string",
              "format": "uuid",
              "pattern": "^[0-9a-f-]{36}$"
            },
            "claim_seq": {
              "type": "integer",
              "minimum": 0,
              "maximum": 9007199254740991
            },
            "actor_client_id": {
              "type": "string",
              "pattern": "^[a-z0-9][a-z0-9._-]{0,127}$"
            },
            "idempotency_key": {
              "type": "string",
              "format": "uuid",
              "pattern": "^[0-9a-f-]{36}$"
            },
            "request_sha256": {
              "type": "string",
              "pattern": "^[0-9a-f]{64}$"
            },
            "imported": {
              "type": "boolean"
            },
            "source": {
              "anyOf": [
                {
                  "type": "object",
                  "properties": {
                    "source_locator": {
                      "type": "string",
                      "maxLength": 4096
                    },
                    "source_sha256": {
                      "type": "string",
                      "pattern": "^[0-9a-f]{64}$"
                    }
                  },
                  "required": [
                    "source_locator",
                    "source_sha256"
                  ],
                  "additionalProperties": false
                },
                {
                  "type": "null"
                }
              ]
            },
            "received_at": {
              "type": "string",
              "format": "date-time"
            },
            "previous_hash": {
              "type": "string",
              "pattern": "^[0-9a-f]{64}$"
            },
            "record_hash": {
              "type": "string",
              "pattern": "^[0-9a-f]{64}$"
            },
            "kind": {
              "const": "completed"
            },
            "payload": {
              "$ref": "#/$defs/Payload_completed"
            }
          },
          "required": [
            "job_id",
            "seq",
            "record_id",
            "store_generation",
            "claim_seq",
            "actor_client_id",
            "idempotency_key",
            "request_sha256",
            "imported",
            "source",
            "received_at",
            "previous_hash",
            "record_hash",
            "kind",
            "payload"
          ],
          "additionalProperties": false
        },
        {
          "type": "object",
          "properties": {
            "job_id": {
              "type": "string",
              "pattern": "^[a-z0-9][a-z0-9._-]{0,127}$"
            },
            "seq": {
              "type": "integer",
              "minimum": 1,
              "maximum": 9007199254740991
            },
            "record_id": {
              "type": "string",
              "format": "uuid",
              "pattern": "^[0-9a-f-]{36}$"
            },
            "store_generation": {
              "type": "string",
              "format": "uuid",
              "pattern": "^[0-9a-f-]{36}$"
            },
            "claim_seq": {
              "type": "integer",
              "minimum": 0,
              "maximum": 9007199254740991
            },
            "actor_client_id": {
              "type": "string",
              "pattern": "^[a-z0-9][a-z0-9._-]{0,127}$"
            },
            "idempotency_key": {
              "type": "string",
              "format": "uuid",
              "pattern": "^[0-9a-f-]{36}$"
            },
            "request_sha256": {
              "type": "string",
              "pattern": "^[0-9a-f]{64}$"
            },
            "imported": {
              "type": "boolean"
            },
            "source": {
              "anyOf": [
                {
                  "type": "object",
                  "properties": {
                    "source_locator": {
                      "type": "string",
                      "maxLength": 4096
                    },
                    "source_sha256": {
                      "type": "string",
                      "pattern": "^[0-9a-f]{64}$"
                    }
                  },
                  "required": [
                    "source_locator",
                    "source_sha256"
                  ],
                  "additionalProperties": false
                },
                {
                  "type": "null"
                }
              ]
            },
            "received_at": {
              "type": "string",
              "format": "date-time"
            },
            "previous_hash": {
              "type": "string",
              "pattern": "^[0-9a-f]{64}$"
            },
            "record_hash": {
              "type": "string",
              "pattern": "^[0-9a-f]{64}$"
            },
            "kind": {
              "const": "grant"
            },
            "payload": {
              "$ref": "#/$defs/Payload_grant"
            }
          },
          "required": [
            "job_id",
            "seq",
            "record_id",
            "store_generation",
            "claim_seq",
            "actor_client_id",
            "idempotency_key",
            "request_sha256",
            "imported",
            "source",
            "received_at",
            "previous_hash",
            "record_hash",
            "kind",
            "payload"
          ],
          "additionalProperties": false
        },
        {
          "type": "object",
          "properties": {
            "job_id": {
              "type": "string",
              "pattern": "^[a-z0-9][a-z0-9._-]{0,127}$"
            },
            "seq": {
              "type": "integer",
              "minimum": 1,
              "maximum": 9007199254740991
            },
            "record_id": {
              "type": "string",
              "format": "uuid",
              "pattern": "^[0-9a-f-]{36}$"
            },
            "store_generation": {
              "type": "string",
              "format": "uuid",
              "pattern": "^[0-9a-f-]{36}$"
            },
            "claim_seq": {
              "type": "integer",
              "minimum": 0,
              "maximum": 9007199254740991
            },
            "actor_client_id": {
              "type": "string",
              "pattern": "^[a-z0-9][a-z0-9._-]{0,127}$"
            },
            "idempotency_key": {
              "type": "string",
              "format": "uuid",
              "pattern": "^[0-9a-f-]{36}$"
            },
            "request_sha256": {
              "type": "string",
              "pattern": "^[0-9a-f]{64}$"
            },
            "imported": {
              "type": "boolean"
            },
            "source": {
              "anyOf": [
                {
                  "type": "object",
                  "properties": {
                    "source_locator": {
                      "type": "string",
                      "maxLength": 4096
                    },
                    "source_sha256": {
                      "type": "string",
                      "pattern": "^[0-9a-f]{64}$"
                    }
                  },
                  "required": [
                    "source_locator",
                    "source_sha256"
                  ],
                  "additionalProperties": false
                },
                {
                  "type": "null"
                }
              ]
            },
            "received_at": {
              "type": "string",
              "format": "date-time"
            },
            "previous_hash": {
              "type": "string",
              "pattern": "^[0-9a-f]{64}$"
            },
            "record_hash": {
              "type": "string",
              "pattern": "^[0-9a-f]{64}$"
            },
            "kind": {
              "const": "grant_cancel"
            },
            "payload": {
              "$ref": "#/$defs/Payload_grant_cancel"
            }
          },
          "required": [
            "job_id",
            "seq",
            "record_id",
            "store_generation",
            "claim_seq",
            "actor_client_id",
            "idempotency_key",
            "request_sha256",
            "imported",
            "source",
            "received_at",
            "previous_hash",
            "record_hash",
            "kind",
            "payload"
          ],
          "additionalProperties": false
        },
        {
          "type": "object",
          "properties": {
            "job_id": {
              "type": "string",
              "pattern": "^[a-z0-9][a-z0-9._-]{0,127}$"
            },
            "seq": {
              "type": "integer",
              "minimum": 1,
              "maximum": 9007199254740991
            },
            "record_id": {
              "type": "string",
              "format": "uuid",
              "pattern": "^[0-9a-f-]{36}$"
            },
            "store_generation": {
              "type": "string",
              "format": "uuid",
              "pattern": "^[0-9a-f-]{36}$"
            },
            "claim_seq": {
              "type": "integer",
              "minimum": 0,
              "maximum": 9007199254740991
            },
            "actor_client_id": {
              "type": "string",
              "pattern": "^[a-z0-9][a-z0-9._-]{0,127}$"
            },
            "idempotency_key": {
              "type": "string",
              "format": "uuid",
              "pattern": "^[0-9a-f-]{36}$"
            },
            "request_sha256": {
              "type": "string",
              "pattern": "^[0-9a-f]{64}$"
            },
            "imported": {
              "type": "boolean"
            },
            "source": {
              "anyOf": [
                {
                  "type": "object",
                  "properties": {
                    "source_locator": {
                      "type": "string",
                      "maxLength": 4096
                    },
                    "source_sha256": {
                      "type": "string",
                      "pattern": "^[0-9a-f]{64}$"
                    }
                  },
                  "required": [
                    "source_locator",
                    "source_sha256"
                  ],
                  "additionalProperties": false
                },
                {
                  "type": "null"
                }
              ]
            },
            "received_at": {
              "type": "string",
              "format": "date-time"
            },
            "previous_hash": {
              "type": "string",
              "pattern": "^[0-9a-f]{64}$"
            },
            "record_hash": {
              "type": "string",
              "pattern": "^[0-9a-f]{64}$"
            },
            "kind": {
              "const": "end"
            },
            "payload": {
              "$ref": "#/$defs/Payload_end"
            }
          },
          "required": [
            "job_id",
            "seq",
            "record_id",
            "store_generation",
            "claim_seq",
            "actor_client_id",
            "idempotency_key",
            "request_sha256",
            "imported",
            "source",
            "received_at",
            "previous_hash",
            "record_hash",
            "kind",
            "payload"
          ],
          "additionalProperties": false
        },
        {
          "type": "object",
          "properties": {
            "job_id": {
              "type": "string",
              "pattern": "^[a-z0-9][a-z0-9._-]{0,127}$"
            },
            "seq": {
              "type": "integer",
              "minimum": 1,
              "maximum": 9007199254740991
            },
            "record_id": {
              "type": "string",
              "format": "uuid",
              "pattern": "^[0-9a-f-]{36}$"
            },
            "store_generation": {
              "type": "string",
              "format": "uuid",
              "pattern": "^[0-9a-f-]{36}$"
            },
            "claim_seq": {
              "type": "integer",
              "minimum": 0,
              "maximum": 9007199254740991
            },
            "actor_client_id": {
              "type": "string",
              "pattern": "^[a-z0-9][a-z0-9._-]{0,127}$"
            },
            "idempotency_key": {
              "type": "string",
              "format": "uuid",
              "pattern": "^[0-9a-f-]{36}$"
            },
            "request_sha256": {
              "type": "string",
              "pattern": "^[0-9a-f]{64}$"
            },
            "imported": {
              "type": "boolean"
            },
            "source": {
              "anyOf": [
                {
                  "type": "object",
                  "properties": {
                    "source_locator": {
                      "type": "string",
                      "maxLength": 4096
                    },
                    "source_sha256": {
                      "type": "string",
                      "pattern": "^[0-9a-f]{64}$"
                    }
                  },
                  "required": [
                    "source_locator",
                    "source_sha256"
                  ],
                  "additionalProperties": false
                },
                {
                  "type": "null"
                }
              ]
            },
            "received_at": {
              "type": "string",
              "format": "date-time"
            },
            "previous_hash": {
              "type": "string",
              "pattern": "^[0-9a-f]{64}$"
            },
            "record_hash": {
              "type": "string",
              "pattern": "^[0-9a-f]{64}$"
            },
            "kind": {
              "const": "import"
            },
            "payload": {
              "$ref": "#/$defs/Payload_import"
            }
          },
          "required": [
            "job_id",
            "seq",
            "record_id",
            "store_generation",
            "claim_seq",
            "actor_client_id",
            "idempotency_key",
            "request_sha256",
            "imported",
            "source",
            "received_at",
            "previous_hash",
            "record_hash",
            "kind",
            "payload"
          ],
          "additionalProperties": false
        },
        {
          "type": "object",
          "properties": {
            "job_id": {
              "type": "string",
              "pattern": "^[a-z0-9][a-z0-9._-]{0,127}$"
            },
            "seq": {
              "type": "integer",
              "minimum": 1,
              "maximum": 9007199254740991
            },
            "record_id": {
              "type": "string",
              "format": "uuid",
              "pattern": "^[0-9a-f-]{36}$"
            },
            "store_generation": {
              "type": "string",
              "format": "uuid",
              "pattern": "^[0-9a-f-]{36}$"
            },
            "claim_seq": {
              "type": "integer",
              "minimum": 0,
              "maximum": 9007199254740991
            },
            "actor_client_id": {
              "type": "string",
              "pattern": "^[a-z0-9][a-z0-9._-]{0,127}$"
            },
            "idempotency_key": {
              "type": "string",
              "format": "uuid",
              "pattern": "^[0-9a-f-]{36}$"
            },
            "request_sha256": {
              "type": "string",
              "pattern": "^[0-9a-f]{64}$"
            },
            "imported": {
              "type": "boolean"
            },
            "source": {
              "anyOf": [
                {
                  "type": "object",
                  "properties": {
                    "source_locator": {
                      "type": "string",
                      "maxLength": 4096
                    },
                    "source_sha256": {
                      "type": "string",
                      "pattern": "^[0-9a-f]{64}$"
                    }
                  },
                  "required": [
                    "source_locator",
                    "source_sha256"
                  ],
                  "additionalProperties": false
                },
                {
                  "type": "null"
                }
              ]
            },
            "received_at": {
              "type": "string",
              "format": "date-time"
            },
            "previous_hash": {
              "type": "string",
              "pattern": "^[0-9a-f]{64}$"
            },
            "record_hash": {
              "type": "string",
              "pattern": "^[0-9a-f]{64}$"
            },
            "kind": {
              "const": "import_begin"
            },
            "payload": {
              "$ref": "#/$defs/Payload_import_begin"
            }
          },
          "required": [
            "job_id",
            "seq",
            "record_id",
            "store_generation",
            "claim_seq",
            "actor_client_id",
            "idempotency_key",
            "request_sha256",
            "imported",
            "source",
            "received_at",
            "previous_hash",
            "record_hash",
            "kind",
            "payload"
          ],
          "additionalProperties": false
        },
        {
          "type": "object",
          "properties": {
            "job_id": {
              "type": "string",
              "pattern": "^[a-z0-9][a-z0-9._-]{0,127}$"
            },
            "seq": {
              "type": "integer",
              "minimum": 1,
              "maximum": 9007199254740991
            },
            "record_id": {
              "type": "string",
              "format": "uuid",
              "pattern": "^[0-9a-f-]{36}$"
            },
            "store_generation": {
              "type": "string",
              "format": "uuid",
              "pattern": "^[0-9a-f-]{36}$"
            },
            "claim_seq": {
              "type": "integer",
              "minimum": 0,
              "maximum": 9007199254740991
            },
            "actor_client_id": {
              "type": "string",
              "pattern": "^[a-z0-9][a-z0-9._-]{0,127}$"
            },
            "idempotency_key": {
              "type": "string",
              "format": "uuid",
              "pattern": "^[0-9a-f-]{36}$"
            },
            "request_sha256": {
              "type": "string",
              "pattern": "^[0-9a-f]{64}$"
            },
            "imported": {
              "type": "boolean"
            },
            "source": {
              "anyOf": [
                {
                  "type": "object",
                  "properties": {
                    "source_locator": {
                      "type": "string",
                      "maxLength": 4096
                    },
                    "source_sha256": {
                      "type": "string",
                      "pattern": "^[0-9a-f]{64}$"
                    }
                  },
                  "required": [
                    "source_locator",
                    "source_sha256"
                  ],
                  "additionalProperties": false
                },
                {
                  "type": "null"
                }
              ]
            },
            "received_at": {
              "type": "string",
              "format": "date-time"
            },
            "previous_hash": {
              "type": "string",
              "pattern": "^[0-9a-f]{64}$"
            },
            "record_hash": {
              "type": "string",
              "pattern": "^[0-9a-f]{64}$"
            },
            "kind": {
              "const": "import_seal"
            },
            "payload": {
              "$ref": "#/$defs/Payload_import_seal"
            }
          },
          "required": [
            "job_id",
            "seq",
            "record_id",
            "store_generation",
            "claim_seq",
            "actor_client_id",
            "idempotency_key",
            "request_sha256",
            "imported",
            "source",
            "received_at",
            "previous_hash",
            "record_hash",
            "kind",
            "payload"
          ],
          "additionalProperties": false
        }
      ]
    },
    "Summary": {
      "type": "object",
      "properties": {
        "job_id": {
          "type": "string",
          "pattern": "^[a-z0-9][a-z0-9._-]{0,127}$"
        },
        "claim_seq": {
          "type": "integer",
          "minimum": 1,
          "maximum": 9007199254740991
        },
        "snapshot_seq": {
          "type": "integer",
          "minimum": 1,
          "maximum": 9007199254740991
        },
        "state": {
          "enum": [
            "shadow",
            "importing",
            "sealed",
            "active",
            "joined",
            "ended"
          ]
        },
        "baseline_used": {
          "type": "integer",
          "minimum": 0,
          "maximum": 9007199254740991
        },
        "used": {
          "type": "integer",
          "minimum": 0,
          "maximum": 9007199254740991
        },
        "cap": {
          "const": 3
        },
        "next_round_no": {
          "type": "integer",
          "minimum": 1,
          "maximum": 9007199254740991
        },
        "open_rounds": {
          "type": "array",
          "items": {
            "type": "integer",
            "minimum": 1,
            "maximum": 9007199254740991
          }
        },
        "last_verdict_seq": {
          "anyOf": [
            {
              "type": "integer",
              "minimum": 1,
              "maximum": 9007199254740991
            },
            {
              "type": "null"
            }
          ]
        },
        "active_grants": {
          "type": "array",
          "items": {
            "type": "integer",
            "minimum": 1,
            "maximum": 9007199254740991
          }
        },
        "warnings": {
          "type": "array",
          "items": {
            "type": "string",
            "pattern": "^[a-z][a-z0-9_]{0,63}$"
          }
        },
        "store_generation": {
          "type": "string",
          "format": "uuid",
          "pattern": "^[0-9a-f-]{36}$"
        },
        "tail_hash": {
          "type": "string",
          "pattern": "^[0-9a-f]{64}$"
        }
      },
      "required": [
        "job_id",
        "claim_seq",
        "snapshot_seq",
        "state",
        "baseline_used",
        "used",
        "cap",
        "next_round_no",
        "open_rounds",
        "last_verdict_seq",
        "active_grants",
        "warnings",
        "store_generation",
        "tail_hash"
      ],
      "additionalProperties": false
    },
    "WriteResponse": {
      "type": "object",
      "properties": {
        "contract": {
          "const": "hk1873.v1"
        },
        "created": {
          "type": "boolean"
        },
        "record": {
          "$ref": "#/$defs/Record"
        },
        "summary": {
          "$ref": "#/$defs/Summary"
        }
      },
      "required": [
        "contract",
        "created",
        "record",
        "summary"
      ],
      "additionalProperties": false
    },
    "JobResponse": {
      "type": "object",
      "properties": {
        "contract": {
          "const": "hk1873.v1"
        },
        "claim": {
          "$ref": "#/$defs/Record"
        },
        "summary": {
          "$ref": "#/$defs/Summary"
        }
      },
      "required": [
        "contract",
        "claim",
        "summary"
      ],
      "additionalProperties": false
    },
    "RecordsResponse": {
      "type": "object",
      "properties": {
        "contract": {
          "const": "hk1873.v1"
        },
        "snapshot_seq": {
          "type": "integer",
          "minimum": 1,
          "maximum": 9007199254740991
        },
        "next_after_seq": {
          "type": "integer",
          "minimum": 0,
          "maximum": 9007199254740991
        },
        "has_more": {
          "type": "boolean"
        },
        "records": {
          "type": "array",
          "items": {
            "$ref": "#/$defs/Record"
          }
        },
        "summary": {
          "$ref": "#/$defs/Summary"
        }
      },
      "required": [
        "contract",
        "snapshot_seq",
        "next_after_seq",
        "has_more",
        "records",
        "summary"
      ],
      "additionalProperties": false
    },
    "ContentResponse": {
      "type": "object",
      "properties": {
        "job_id": {
          "type": "string",
          "pattern": "^[a-z0-9][a-z0-9._-]{0,127}$"
        },
        "seq": {
          "type": "integer",
          "minimum": 1,
          "maximum": 9007199254740991
        },
        "content_b64": {
          "type": "string",
          "contentEncoding": "base64",
          "minLength": 4,
          "maxLength": 349528
        },
        "content_sha256": {
          "type": "string",
          "pattern": "^[0-9a-f]{64}$"
        },
        "size_bytes": {
          "type": "integer",
          "minimum": 1,
          "maximum": 9007199254740991
        }
      },
      "required": [
        "job_id",
        "seq",
        "content_b64",
        "content_sha256",
        "size_bytes"
      ],
      "additionalProperties": false
    },
    "Capabilities": {
      "type": "object",
      "properties": {
        "contract": {
          "const": "hk1873.v1"
        },
        "store_generation": {
          "type": "string",
          "format": "uuid",
          "pattern": "^[0-9a-f-]{36}$"
        },
        "client_id": {
          "type": "string",
          "pattern": "^[a-z0-9][a-z0-9._-]{0,127}$"
        },
        "policy_sha256": {
          "type": "string",
          "pattern": "^[0-9a-f]{64}$"
        },
        "writes_available": {
          "type": "boolean"
        }
      },
      "required": [
        "contract",
        "store_generation",
        "client_id",
        "policy_sha256",
        "writes_available"
      ],
      "additionalProperties": false
    },
    "Error": {
      "type": "object",
      "properties": {
        "error": {
          "enum": [
            "invalid_request",
            "invalid_cursor",
            "invalid_verdict",
            "invalid_path",
            "grant_before_cap",
            "unauthorized",
            "scope_denied",
            "host_mismatch",
            "not_found",
            "idempotency_conflict",
            "sequence_conflict",
            "claim_conflict",
            "job_ended",
            "review_sealed",
            "round_cap",
            "round_not_open",
            "round_has_verdict",
            "round_voided",
            "head_mismatch",
            "duplicate_path",
            "duplicate_content",
            "duplicate_head",
            "grant_invalid",
            "grant_consumed",
            "grant_expired",
            "zero_rounds",
            "review_incomplete",
            "blocker_unresolved",
            "wrk_receipt_required",
            "import_unsealed",
            "request_too_large",
            "unsupported_media_type",
            "wrk_unavailable",
            "wrk_recovery_required",
            "method_not_allowed",
            "import_sealed",
            "migration_frozen"
          ]
        }
      },
      "required": [
        "error"
      ],
      "additionalProperties": false
    },
    "Input_import_begin": {
      "type": "object",
      "properties": {
        "reason_code": {
          "type": "string",
          "pattern": "^[a-z][a-z0-9_]{0,63}$"
        }
      },
      "required": [
        "reason_code"
      ],
      "additionalProperties": false
    },
    "Input_import_seal": {
      "type": "object",
      "properties": {
        "begin_seq": {
          "type": "integer",
          "minimum": 1,
          "maximum": 9007199254740991
        },
        "expected_import_count": {
          "type": "integer",
          "minimum": 0,
          "maximum": 9007199254740991
        },
        "manifest_sha256": {
          "type": "string",
          "pattern": "^[0-9a-f]{64}$"
        },
        "reason_code": {
          "type": "string",
          "pattern": "^[a-z][a-z0-9_]{0,63}$"
        }
      },
      "required": [
        "begin_seq",
        "expected_import_count",
        "manifest_sha256",
        "reason_code"
      ],
      "additionalProperties": false
    },
    "Payload_import_begin": {
      "type": "object",
      "properties": {
        "reason_code": {
          "type": "string",
          "pattern": "^[a-z][a-z0-9_]{0,63}$"
        },
        "shadow_cutoff_seq": {
          "type": "integer",
          "minimum": 1,
          "maximum": 9007199254740991
        }
      },
      "required": [
        "reason_code",
        "shadow_cutoff_seq"
      ],
      "additionalProperties": false
    },
    "Payload_import_seal": {
      "type": "object",
      "properties": {
        "begin_seq": {
          "type": "integer",
          "minimum": 1,
          "maximum": 9007199254740991
        },
        "expected_import_count": {
          "type": "integer",
          "minimum": 0,
          "maximum": 9007199254740991
        },
        "manifest_sha256": {
          "type": "string",
          "pattern": "^[0-9a-f]{64}$"
        },
        "reason_code": {
          "type": "string",
          "pattern": "^[a-z][a-z0-9_]{0,63}$"
        },
        "shadow_cutoff_seq": {
          "type": "integer",
          "minimum": 1,
          "maximum": 9007199254740991
        },
        "baseline_used": {
          "type": "integer",
          "minimum": 0,
          "maximum": 9007199254740991
        },
        "numbering_floor": {
          "type": "integer",
          "minimum": 0,
          "maximum": 9007199254740991
        }
      },
      "required": [
        "begin_seq",
        "expected_import_count",
        "manifest_sha256",
        "reason_code",
        "shadow_cutoff_seq",
        "baseline_used",
        "numbering_floor"
      ],
      "additionalProperties": false
    },
    "Policy": {
      "type": "object",
      "properties": {
        "version": {
          "const": "hk1873.policy.v1"
        },
        "clients": {
          "type": "object",
          "properties": {
            "desktop": {
              "type": "object",
              "properties": {
                "host_client_id": {
                  "const": "desktop"
                },
                "permissions": {
                  "type": "array",
                  "items": {
                    "enum": [
                      "capabilities.read",
                      "job.read",
                      "records.read",
                      "content.read",
                      "round.open",
                      "verdict.write",
                      "void.write",
                      "joined.write",
                      "completed.write",
                      "end.write",
                      "relay.publish",
                      "reviewer.claim.write"
                    ]
                  },
                  "uniqueItems": true
                }
              },
              "required": [
                "host_client_id",
                "permissions"
              ],
              "additionalProperties": false
            },
            "mac-dev": {
              "type": "object",
              "properties": {
                "host_client_id": {
                  "const": "mac-dev"
                },
                "permissions": {
                  "type": "array",
                  "items": {
                    "enum": [
                      "capabilities.read",
                      "job.read",
                      "records.read",
                      "content.read",
                      "round.open",
                      "verdict.write",
                      "void.write",
                      "joined.write",
                      "completed.write",
                      "end.write",
                      "relay.publish",
                      "reviewer.claim.write"
                    ]
                  },
                  "uniqueItems": true
                }
              },
              "required": [
                "host_client_id",
                "permissions"
              ],
              "additionalProperties": false
            },
            "mac-work": {
              "type": "object",
              "properties": {
                "host_client_id": {
                  "const": "mac-work"
                },
                "permissions": {
                  "type": "array",
                  "items": {
                    "enum": [
                      "capabilities.read",
                      "job.read",
                      "records.read",
                      "content.read",
                      "round.open",
                      "verdict.write",
                      "void.write",
                      "joined.write",
                      "completed.write",
                      "end.write",
                      "relay.publish",
                      "reviewer.claim.write"
                    ]
                  },
                  "uniqueItems": true
                }
              },
              "required": [
                "host_client_id",
                "permissions"
              ],
              "additionalProperties": false
            },
            "rpi": {
              "type": "object",
              "properties": {
                "host_client_id": {
                  "const": "rpi"
                },
                "permissions": {
                  "type": "array",
                  "items": {
                    "enum": [
                      "capabilities.read",
                      "job.read",
                      "records.read",
                      "content.read",
                      "round.open",
                      "verdict.write",
                      "void.write",
                      "joined.write",
                      "completed.write",
                      "end.write",
                      "relay.publish",
                      "reviewer.claim.write"
                    ]
                  },
                  "uniqueItems": true
                }
              },
              "required": [
                "host_client_id",
                "permissions"
              ],
              "additionalProperties": false
            },
            "mac-personal": {
              "type": "object",
              "properties": {
                "host_client_id": {
                  "const": "mac-personal"
                },
                "permissions": {
                  "type": "array",
                  "items": {
                    "enum": [
                      "capabilities.read",
                      "job.read",
                      "records.read",
                      "content.read",
                      "round.open",
                      "verdict.write",
                      "void.write",
                      "joined.write",
                      "completed.write",
                      "end.write",
                      "relay.publish",
                      "reviewer.claim.write"
                    ]
                  },
                  "uniqueItems": true
                }
              },
              "required": [
                "host_client_id",
                "permissions"
              ],
              "additionalProperties": false
            },
            "director-1": {
              "type": "object",
              "properties": {
                "host_client_id": {
                  "type": "null"
                },
                "permissions": {
                  "type": "array",
                  "items": {
                    "enum": [
                      "capabilities.read",
                      "job.read",
                      "records.read",
                      "content.read",
                      "claim.write",
                      "activate.write",
                      "grant.write",
                      "grant.cancel",
                      "end.write",
                      "import.begin",
                      "import.seal",
                      "import.write",
                      "task.state",
                      "reviewer.claim.write"
                    ]
                  },
                  "uniqueItems": true
                }
              },
              "required": [
                "host_client_id",
                "permissions"
              ],
              "additionalProperties": false
            },
            "pa-integrator": {
              "type": "object",
              "properties": {
                "host_client_id": {
                  "type": "null"
                },
                "permissions": {
                  "type": "array",
                  "items": {
                    "enum": [
                      "capabilities.read",
                      "job.read",
                      "records.read",
                      "content.read"
                    ]
                  },
                  "uniqueItems": true
                }
              },
              "required": [
                "host_client_id",
                "permissions"
              ],
              "additionalProperties": false
            },
            "operator-desk": {
              "type": "object",
              "properties": {
                "host_client_id": {
                  "type": "null"
                },
                "permissions": {
                  "type": "array",
                  "items": {
                    "enum": [
                      "capabilities.read",
                      "job.read",
                      "records.read",
                      "content.read",
                      "claim.write",
                      "activate.write",
                      "grant.write",
                      "grant.cancel",
                      "end.write",
                      "import.begin",
                      "import.seal",
                      "import.write",
                      "task.state",
                      "reviewer.claim.write"
                    ]
                  },
                  "uniqueItems": true
                }
              },
              "required": [
                "host_client_id",
                "permissions"
              ],
              "additionalProperties": false
            },
            "at-integrator": {
              "type": "object",
              "properties": {
                "host_client_id": {
                  "type": "null"
                },
                "permissions": {
                  "type": "array",
                  "items": {
                    "enum": [
                      "capabilities.read",
                      "job.read",
                      "records.read",
                      "content.read"
                    ]
                  },
                  "uniqueItems": true
                }
              },
              "required": [
                "host_client_id",
                "permissions"
              ],
              "additionalProperties": false
            },
            "pb-integrator": {
              "type": "object",
              "properties": {
                "host_client_id": {
                  "type": "null"
                },
                "permissions": {
                  "type": "array",
                  "items": {
                    "enum": [
                      "capabilities.read",
                      "job.read",
                      "records.read",
                      "content.read"
                    ]
                  },
                  "uniqueItems": true
                }
              },
              "required": [
                "host_client_id",
                "permissions"
              ],
              "additionalProperties": false
            },
            "tt-integrator": {
              "type": "object",
              "properties": {
                "host_client_id": {
                  "type": "null"
                },
                "permissions": {
                  "type": "array",
                  "items": {
                    "enum": [
                      "capabilities.read",
                      "job.read",
                      "records.read",
                      "content.read"
                    ]
                  },
                  "uniqueItems": true
                }
              },
              "required": [
                "host_client_id",
                "permissions"
              ],
              "additionalProperties": false
            }
          },
          "required": [
            "desktop",
            "mac-dev",
            "mac-work",
            "rpi",
            "mac-personal",
            "director-1",
            "pa-integrator",
            "operator-desk",
            "at-integrator",
            "pb-integrator",
            "tt-integrator"
          ],
          "additionalProperties": false
        },
        "lanes": {
          "type": "object",
          "minProperties": 1,
          "propertyNames": {
            "type": "string",
            "pattern": "^[a-z0-9][a-z0-9-]{0,63}$"
          },
          "additionalProperties": {
            "anyOf": [
              {
                "type": "string",
                "pattern": "^[a-z0-9][a-z0-9-]{0,63}$"
              },
              {
                "type": "null"
              }
            ]
          }
        },
        "features": {
          "type": "object",
          "properties": {
            "head_repeat_enabled": {
              "const": true
            },
            "claim_lane_creation_enabled": {
              "type": "boolean"
            }
          },
          "required": [
            "head_repeat_enabled",
            "claim_lane_creation_enabled"
          ],
          "additionalProperties": false
        }
      },
      "required": [
        "version",
        "clients",
        "lanes",
        "features"
      ],
      "additionalProperties": false
    }
  },
  "policy_schema": {
    "$ref": "#/$defs/Policy"
  }
}
~~~

Additional wire invariants: Content-Length or streamed body above 1048576 bytes returns request_too_large. Decoded evidence/import content above 262144 bytes returns request_too_large. Invalid base64, empty content, mismatched asserted SHA, unknown query keys, unknown JSON fields or duplicate keys return invalid_request, except malformed/multiple verdict lines return invalid_verdict. All endpoints authenticate first. Unsupported methods return 405 with {"error":"method_not_allowed"}; unknown /v1/wrk route returns 404 not_found. Read endpoints allow no mutation fields and use Cache-Control: no-store. All errors_by_status entries apply where their predicate exists; method_not_allowed is included in the bundle's Error enum. Responses never contain a Location redirect. Legacy receipt schema is exactly {"job_id":job-id,"seq":positive-int,"record_hash":sha256,"store_generation":uuid}, under the optional field wrk_receipt of a legacy relay request/local projection. It is mandatory for protected jobs; the server compares it to the canonical record and does not trust a caller-supplied hash without that lookup.

Grant field cross-constraints: extension requires round_no=next_round_no, head non-null, extra=1, earlier_round_no=null, tester_job_id=null. void requires an existing round_no, head=null, extra=0, earlier_round_no=null, tester_job_id=null. head_repeat requires round_no=next_round_no, head non-null, extra=0, earlier_round_no an earlier round on that head, tester_job_id non-null and an existing server-accepted reviewer for this job/round. join_waiver requires round_no=null, head non-null, extra=0, earlier_round_no=null, tester_job_id=null. expires_at must be later than server received_at and no more than seven days ahead at grant creation; consumption before expiry uses server time. Two distinct grant ids may be needed for an over-cap repeated-head review: extension and head_repeat. Supplying any irrelevant non-null grant field is grant_invalid. If round_open.tester_job_id is non-null the matching server-accepted review claim must exist and be active at open acceptance; a precreated review claim may name a future next_round_no. Otherwise a later reviewer claim can attach via verdict.tester_job_id, whose review_of must match. Host-owned V submission without a tester id is explicitly host-attested only. A cross-host V submission always requires a matching tester id/review_of; a host cannot write a foreign job merely by asserting a reviewer name.

Activation is accepted only with server WRK_RECORDS_API=enforce; otherwise it returns 503 wrk_unavailable. An initial claim's job_id must equal the path and no record may precede it. claim_seq=0/expected_seq=0 are only for first claim; subsequent input claim_seq refers to the latest claim record. Root builder claim creation atomically binds the task; an already linked conflicting task is claim_conflict. Tester claims inherit task_id and do not change the root task's refs. owner_lane and parent_lane must be valid existing/allowed canonical lanes; builder/worker edges must match the lane graph, while review-role ownership/routing follows the explicit §2 rule and creates no edge. A claim correction before activation can replace a mistaken binding in the same transaction. Superseded bindings are preserved as history. Protected task enrollment is durable server metadata, not task refs that a host can overwrite.

For preactivation shadow builder/worker subjects, normal writes execute the same validation rules but are only observations. This statement does not apply to a canonical review-role job's own completed/end lifecycle: those are receipt-required from claim under §2, even while its summary.state is shadow. The explicit freeze/seal rules below replace any informal pause. Activation sets the baseline to at least 3 for preactivation work evidence, even without import records; claims alone are excluded. The live used formula counts only round opens AFTER activate plus that baseline, avoiding double counting. Pre-activation verdict records remain negative evidence for path/content/head replay checks but cannot satisfy join/PASS. activated jobs can never accept imports, claim corrections or another activate. Their state is active until joined/end. Preactivation opens are never live-pending after activation, cannot receive a post-activation verdict, and cannot be voided to reduce the baseline. Immutable content reads also support import, returning the archived bytes; import payload listings omit content_b64. import.source is {source_locator,source_sha256}; other kinds have source=null. completed and joined store report bytes; report_sha256 is recomputed and matched just as for verdict. payload.created_at is only on claim and equals its received_at. The active-grants summary lists only unexpired, uncancelled, unconsumed grants whose frozen target is still usable in the current job state; a stranded target produces grant_stranded warning and is excluded. No grant capacity is refunded. Successful replay returns created=false and the original committed summary; callers wanting current state must GET it.

Minimum cross-implementation transition vectors (fresh, centrally claimed and activated job A, host desktop, head H, distinct content/path per verdict; each step reads current seq and uses a new key unless stated):

| Vector | Expected result |
|---|---|
| Open H, submit PASS H bytes with asserted matching digest | 201 round_open round_no=1, 201 verdict bound to 1; used=1 |
| Retry that verdict with identical successful key/body after end | 200 created=false, identical record id/hash, no new seq |
| Same successful key, changed path | 409 idempotency_conflict, no new business record |
| Two simultaneous opens at used=2 with same expected_seq | One 201 used=3, other 409 sequence_conflict; refreshed retry without grant gets round_cap |
| PASS H2 submitted for open H1 | 409 head_mismatch; open remains pending |
| Same path with different bytes/head; same bytes at fresh path | duplicate_path; duplicate_content, respectively |
| Second open H with no repeat grant; with valid repeat grant for reviewer B | duplicate_head; 201, separate numbered round charged against cap |
| Open beyond cap with valid extension grant; reuse it for another open | 201 with grant receipt; grant_consumed (or sequence_conflict if caller uses a stale seq) |
| Void a verdict-bearing round | round_has_verdict, no consumed grant |
| Void an open with a valid void grant, then submit verdict for it | 201 void (used decremented), then round_voided |
| Submit to foreign-host job without central review assignment | host_mismatch, no business record |
| Import locally planted PASS/grant, activate, request joined | No trusted PASS/grant; baseline>=3; zero_rounds without waiver; a fresh review needs extension |
| Submit any new mutation after end | job_ended; successful prior-key replay remains 200 |
| Page limit=1 at snapshot S, append a new record during paging | Full seq 1..S exactly once across pages; new record excluded |
| forged legacy relay / task no-job bypass for an enforced job/task | wrk_receipt_required and no terminal projection |
| Assert content digest D while bytes hash to E | invalid_request, no verdict or consumed grant |

R2 normative policy, migration and wire details (part of section 5):

Policy is validated against $defs.Policy. All eleven client entries (the original eight plus Q4 attribution ids) and all feature keys are required; an empty permissions array means deny all. No other client key, permission name or property is allowed. A missing/invalid file fails startup when the O3 API is enabled; off-mode can keep legacy endpoints running but cannot enroll jobs. policy_sha256 is lowercase SHA256 over the RFC8785 canonical UTF-8 JSON of the complete parsed policy object, without a trailing newline. Whitespace or object-key order does not change the digest; array order does. Duplicate JSON keys are rejected before hashing. A policy reload is atomic: invalid replacement retains the prior validated snapshot and raises an operator diagnostic without echoing its contents. Runtime API clients cannot reload or edit the policy.

The complete permission-to-operation map is:

| Permission name | Matrix operation |
|---|---|
| capabilities.read | Read capabilities |
| job.read | Read canonical claim/summary |
| records.read | Read paginated records, including import_begin/import_seal |
| content.read | Read immutable content |
| claim.write | Write root/builder/worker claim, correction or successor; only director-1/operator-desk |
| reviewer.claim.write | Write a review assignment claim; scoped H for hosts, A for director-1/operator-desk, §2 exact role rule |
| activate.write | Write activate; only director-1/operator-desk |
| round.open | Write round_open; H rule |
| verdict.write | Write verdict; V rule |
| void.write | Write void; H rule |
| joined.write | Write joined; H rule |
| completed.write | Write completed; H rule |
| grant.write | Write grant; only director-1/operator-desk |
| grant.cancel | Write grant_cancel; only director-1/operator-desk |
| end.write | Write end; H rule or central A |
| import.begin | Write import_begin; only director-1/operator-desk |
| import.seal | Write import_seal; only director-1/operator-desk |
| import.write | Write import; only director-1/operator-desk |
| relay.publish | Publish a legacy relay with canonical receipt; H rule |
| task.state | State-only protected-task transition with matching receipt; central only |

Fixed ceilings: all eleven listed ids may have the four .read permissions. Each of the five host ids may additionally have reviewer.claim.write, round.open, verdict.write, void.write, joined.write, completed.write, end.write and relay.publish; its host_client_id MUST equal its client id. director-1/operator-desk may additionally have reviewer.claim.write, claim.write, activate.write, grant.write, grant.cancel, end.write, import.begin, import.seal, import.write and task.state; host_client_id MUST be null. pa-integrator, at-integrator, pb-integrator and tt-integrator have only the four reads and host_client_id=null. Any policy permission outside that client's ceiling or an incorrect host mapping is invalid policy, never an expansion of the matrix. Audit writes/reads and legacy binding mutations have no public permission spelling. The installed policy gives each id its full allowed ceiling; attenuation is explicit removal from its list. Q2=a requires features.head_repeat_enabled=true (const true in Policy); disabling it is invalid policy, not a runtime fallback. features.claim_lane_creation_enabled is true in the installed policy. Both keys are required, not omitted-key defaults. The nonsecret root lane map is at minimum {"director-1":null,"pa-integrator":"director-1"}.

Lane graph serialization: every claim creation/correction and policy reload that validates or changes a lane edge first acquires PostgreSQL transaction advisory lock 8241873001, before all lexicographically ordered job locks. All such writers use this same lock and re-read the canonical claim graph/dependents inside the transaction after acquiring it. Hold it through claim commit or atomic policy publication. Job-only locks never precede this global lock on those paths. Thus concurrent claims for different jobs cannot establish conflicting parents, and the no-dependent-claim supersede check cannot race a new child claim. Policy reload validates every referenced canonical edge under the same lock; fleet deployment requires identical loaded policy digests across enabled writers. Lane edges are journal projections; no unfenced writer or separate mutable lane table is permitted.

Lane graph algorithm: policy.lanes maps a configured lane name to its parent (null for a root), must include director-1 as a root, and must have no cycles or missing parents. Canonical builder claims may introduce a fresh owner_lane with their parent_lane when claim_lane_creation_enabled=true and the parent is already a configured node or a canonical builder lane. If the owner already exists, its parent must equal parent_lane. Nonbuilder claims cannot introduce a lane. Non-review workers use their owning builder lane and its existing parent. Review-role claims use the role-specific pair owner_lane=parent_lane=reviewed claim.owner_lane described in §2; it is ownership/routing data, not a graph edge. review_of fixes their target task/round, and minting cannot change that lane or task. This derived graph lives in the canonical claim journal, not in mutable task refs or a host file. Canonical claims supersede old claim edges before activation only when no other claim depends on the old edge; otherwise claim_conflict and a new uniquely named lane are required. A reload cannot remove/change a node/edge referenced by a live canonical claim. Unknown parents, cycles, conflicting parent edges and attempted root overwrite are claim_conflict. These checks apply before central claim commit; an authoritative client cannot smuggle an unrelated lane by a different route.

Minimal policy tests: an authenticated id absent from the fixed eleven returns scope_denied on every O3 route, including reads. A policy containing permission "grant.any" fails validation and cannot start an enabled O3 API; it does not silently ignore the typo. A desktop entry containing grant.write also fails validation. Canonicalization of the same policy with reordered keys yields identical policy_sha256 and identical claim policy digest.

Import freeze/seal state machine and accounting:

- State shadow starts after first claim. Work evidence means any preactivation round_open, verdict, void, completed, joined, grant or grant_cancel record, or any import record. Claim/claim corrections and import_begin/import_seal control records alone are NOT work evidence. Fresh claim-only jobs may activate directly with payload.seal_seq=null and baseline_used=0, or predecessor.used for a trusted successor. Any work evidence requires a completed seal; missing/wrong seal is import_unsealed. An already joined/ended shadow job cannot be activated; it is archived as legacy and recovery uses a successor.
- import_begin is central-only, shadow-only, once per job. Its payload is {reason_code}; persisted payload adds shadow_cutoff_seq=begin.seq-1. The commit changes state to importing. Afterward every host mutation returns migration_frozen, with no business record; reads and exact successful idempotency replays remain available. Centrally minted records during importing are restricted to import, import_seal and end. Grants and claim corrections are also frozen. All already committed shadow work up to the cutoff is included in the sealed computation. CAS means a racing host write either lands before the cutoff or loses/fails; it cannot interleave after the freeze.
- import stores exact archived event bytes plus asserted provenance as specified above. It is accepted only in importing. Neither the source locator nor asserted actor is trusted authority. The server parses legacy event bytes as JSON only to compute conservative budget/negative replay evidence; malformed/unrecognized data stays archived and adds migration_history_incomplete. It never becomes a positive verdict/grant/claim.
- import_seal is central-only and importing-only, once per job. Its request fields are begin_seq, expected_import_count, manifest_sha256 and reason_code. Build the manifest as an array, ascending import record seq, of {seq,source_locator,source_sha256} for ALL import records in this job between begin and seal. Its hash is SHA256 of RFC8785 canonical JSON of that array, without newline. Count/hash mismatch, a wrong begin_seq or no begin produces import_unsealed. The persisted seal adds shadow_cutoff_seq, baseline_used and numbering_floor. It changes state to sealed. Nothing except activate or end may append after a seal; other new writes return import_sealed. A failed seal leaves state importing so missing archive records can be appended and the manifest recomputed.
- Compute L as the number of imported legacy job.round plus imported legacy job.verdict records with absent/null/noninteger round, PLUS the number of server shadow round_open records and server shadow verdict records with no valid bound round. Canonical shadow verdicts normally cannot be unbound. Count all these observations conservatively, including duplicated local/server views; duplicate counting costs slots and cannot grant authority. Compute I as predecessor.used if any plus the maximum valid nonnegative integer payload.rounds_used found in archived job.takeover events, or zero when absent; asserted legacy inherited values can only raise the floor. baseline_used=max(3,L+I) if work evidence exists, otherwise I. Ignore all shadow/imported void reductions. Safe-integer overflow is invalid_request and blocks activation. numbering_floor=max(baseline_used, all valid positive round numbers found in shadow/imported round/verdict/void records). A missing/nonparseable history cannot lower the base-cap reservation. Only a zero-work job can retain zero baseline.
- activate with seal_seq equal to the committed seal and expected_seq equal to the current tail uses the stored baseline/numbering_floor; it cannot recompute a smaller snapshot. It sets state active. No imports/claims/seals/second activate are then allowed. A host cannot bypass the seal by sending seal_seq=null after work evidence. A seal pins the archive actually supplied; it does not certify completeness of builder-controlled history. The floor and fresh review requirement are the protection against omissions, not the manifest hash.
- Once active, live used = sealed baseline_used + number of post-activate round_open records minus post-activate verdict-free voids of those same post-activate rounds. The baseline never decreases. Historical round opens are NOT pending. A new verdict or void targeting any preactivate round returns round_not_open, even if its old shadow open had no verdict. Historical PASS never satisfies join. They remain listed with original seq and shadow provenance. The next round number is 1+max(numbering_floor, authoritative post-activate round numbers); no old ordinal is recycled. Path/content/head negative identity from parseable legacy/shadow verdicts remains. At activation, every preactivation open head reservation expires, whether or not it was voided; historical opens never reserve a head. If a historical verdict exists on H, Q2=a enables a fresh review on H only with an exact centrally issued head_repeat grant (plus extension at the imported baseline). Without those grants the new open is refused; the historical PASS remains untrusted. Fresh grants are required for new over-cap work; preactivate grants are historical and cannot be consumed.
- Except for an explicit valid zero-round join_waiver, T2/T3 join requires fresh server-held PASS bytes plus its canonical reviewer assignment validated when the PASS was accepted; the reviewer may have ended or been reaped since then. Existing shadow opens do not block it. Normal live pending opens do. A live void reduces only the live-open component of used; it never reduces the sealed baseline. All imports and preactivation metadata remain visible to readers.

New exact errors: migration_frozen is 409 for host mutations during importing (or a CAS-resolved attempt against that frozen state); import_sealed is 409 for disallowed mutations in sealed state; import_unsealed is 409 for invalid/missing manifest/seal or activation without a required seal. Business error ordering still starts with successful idempotency replay and CAS; stale expected_seq may therefore produce sequence_conflict before the migration predicate. End remains final. §4's universal no-writes-after-end rule overrides migration operations.

Additional migration vectors:

| Initial history and request | Exact expected result |
|---|---|
| seq1 claim; activate with seal_seq=null | 201 baseline_used=0, active, used=0, next_round_no=1 |
| seq1 claim; seq2 shadow open 1/H; activate without seal | 409 import_unsealed |
| Same shadow history; begin at seq3; racing host verdict after begin | 409 migration_frozen after refresh; no verdict, old round in frozen snapshot |
| Same history; seal with zero-import canonical [] manifest; activate exact seal | baseline_used=3, numbering_floor=3, active, used=3, open_rounds=[] |
| After that activate, submit verdict or void for old round 1 | 409 round_not_open; used remains 3, no grant consumed |
| Same active job; central extension for next round 4/H, plus head_repeat if historical verdict reserved H; fresh round 4/PASS from assigned tester | used=4; fresh PASS can satisfy join; old round 1 is not pending |
| Void postactivate round 4 before verdict with valid grant | used=3, never 2; baseline remains 3 |
| Manifest claims one import but server has two | import_unsealed; still importing; no activation |

Request digest/idempotency: request_sha256 = SHA256(RFC8785({"method":"POST","path":"/v1/wrk/jobs/"+job_id+"/records","body":the_entire_parsed_Request_object})). Path uses the validated ASCII job id, no percent-encoding aliases; kind, claim_seq and expected_seq are included inside body. Idempotency-Key and bearer headers are excluded; cache scope is authenticated actor/job/key. A retry after a lost reply MUST resend the identical body, including original expected_seq. Changing expected_seq after a successful commit while retaining the key returns idempotency_conflict, even if payload is otherwise identical. The identical original retry returns 200 before CAS, even when current tail advanced. After a known sequence_conflict (no success cached), the client may refresh and use a new key/body. The server never guesses that a changed request was a retry.

Verdict byte grammar: decode exact stored bytes as strict UTF-8, reject a leading BOM, split physical lines only at LF, and match first line using ASCII regex ^VERDICT: (PASS|BLOCKER) @([0-9a-fA-F]{40})[ \t]*\r?$. There must be exactly one full match in the file and no later physical line beginning VERDICT: (even if malformed). Trailing LF is optional; CRLF and trailing ASCII space/tab are accepted and hashed unchanged. Lowercase the parsed SHA before comparison to the lowercase asserted request head. A leading BOM, invalid UTF-8 anywhere, a nonmatching first/header line or any later physical line beginning VERDICT: is invalid_verdict. Header whitespace is ASCII space/tab only; non-ASCII whitespace or a bare-CR separator in the FIRST LINE is invalid_verdict. The header may end with LF, CRLF or no terminator; a CR at its end is valid only when followed by the first LF. These header-only whitespace/CR rules do not restrict the UTF-8 body: body U+00A0, U+3000 or embedded CR is allowed, subject to the content guard and the no-extra-VERDICT-prefix rule. Examples: lowercase/uppercase 40-hex with LF or CRLF pass (assertion normalized lowercase); a preamble before VERDICT, UTF-8 BOM, 41 hex digits and two VERDICT lines fail. This intentionally tightens the current anywhere-in-file parser (AS bin/wrk:4591,6094–6108); legacy files are archived rather than silently reformatted or accepted. Reviewer files must be regenerated with first-line verdicts for live O3 submission.

Head-repeat binding vector: a grant for reviewer B cannot be consumed with round_open.tester_job_id=null or reviewer C; both return grant_invalid. A valid exact-B open followed by verdict.tester_job_id=null/C fails the assignment check. A reviewer claim ended/reaped before new verdict append gives round_not_open for that assignment; exact successful replay remains available. A reviewer ending after accepted PASS has no effect on later joined acceptance. Aborted live verdict-free open -> valid void -> new same-head open is allowed without head_repeat if no other live open or actual verdict reserves the head. The void does not allow copying a prior verdict path/bytes.

Legacy route fence coverage (all surfaces use the same service-layer principal/receipt check, including HTTP, MCP, assistant and operator UI; a caller-supplied by string is never an authenticated central role):

| Current route at HK internal/api/api.go:431–475 | O3 classification |
|---|---|
| POST /v1/tasks | Creating an unrelated legacy task remains legacy-compatible. A supplied refs.job_id pointing to an O3 job or protected task association is rejected with wrk_receipt_required; this route cannot enroll or relink authority. |
| POST /v1/tasks/next | Protected tasks excluded from legacy claiming; explicit attempt to claim one is wrk_receipt_required. Central O3 claim transaction owns binding. |
| POST /v1/tasks/{id}/claim and /rebind | Every protected binding mutation refused, including central, no_job and retry variants. Use canonical claim/correction/successor transaction. |
| POST /v1/tasks/{id}/transition | Protected refs/claimed_by/job binding changes always refused. State-only changes require central task.state and receipt: canonical claim for nonterminal states, canonical joined for join/merged, canonical end for dropped; every receipt must name the task's current bound subject job/current claim. A reviewer end sharing task_id is insufficient. Host ids denied. |
| POST /v1/tasks/{id}/project and POST /v1/tasks/relane | Cannot change protected task's identity/binding/parent or project through legacy route; wrk_receipt_required. Parent/task for authority remains frozen in claim. Unrelated legacy tasks keep old behaviour. |
| POST /v1/tasks/{id}/decision-request/resolve | Protected state-only resumption requires central principal + current claim receipt; supplied refs/binding changes refused. |
| POST /v1/tasks/dispositions/{id}/apply | If protected, action that changes binding is refused; state-only action uses same central/receipt terminal rules. |
| POST /v1/decisions/resolve and /v1/assistant/decisions/resolve | Cannot bypass protected-task central/receipt transition rules, including no_job and resumed claimed state. Non-task textual lane notifications remain notifications. |
| POST /v1/relay/events | Every §2 receipt_required job (including a review-role job in summary.state shadow) requires the exact canonical receipt for job.completed/job.joined/end; only matching projection/dedup is accepted. lane.event text never authorizes completion, routing authority or quota release. |

An old API with a free by string cannot prove director-1; handlers must pass an authenticated principal to the service. Operator UI/MCP/assistant calls without a bearer-derived O3 principal have no O3 mutation authority; they return scope_denied for protected writes. No implicit UI-to-operator-desk mapping is introduced. Unprotected legacy operations keep existing behaviour. The canonical record and its terminal relay projection commit together; legacy retries cannot create a second authority record. Binding mutation denial is the same for central and host clients; the state-only permission is intentionally separate in the matrix.

Client history checks are explicit: before trusting any new summary, compare store_generation and snapshot_seq against retained receipts; if a retained seq is within the returned history, fetch that exact record and compare record_hash. A lower seq, changed generation, absent retained record or unequal hash is local history_regressed and stops authority use. This name is not in errors_by_status; the server cannot know unreported external witness data. Operator recovery mode produces server 503 wrk_recovery_required. Report copying across unrelated readable jobs can still produce host-attested evidence on an already authorized same head; predecessor linkage and canonical T2/T3 assignments narrow it, but truthful review remains outside the host token boundary.


R3 regression vectors (normative, in addition to the earlier vectors):

| History/request | Exact expected result |
|---|---|
| T2 A has matching assigned reviewer T; T is active at named round_open and PASS append; T then receives end reaped; A submits joined with all other gates met | 201 joined. Do not test current T activeness at join; accepted PASS remains evidence. No extra review/extension is needed for tester cleanup. |
| Zero authoritative rounds; exact-head one-use join_waiver; T2 join, all other gates met | 201 joined with waiver_grant_seq, no PASS/tester assignment fabricated; director explicitly accepts untested completion. |
| T ends before its named open or before verdict append | round_not_open for the unavailable assignment, no new verdict; this does not affect earlier accepted evidence. |
| Shadow open H without a verdict; import freeze/seal/activate; extension for new postactivation round H | New round on H allowed without head_repeat; old open is historical and has no head reservation. |
| Historical verdict on H; activation; no repeat grant; new open H | duplicate_head. Q2=a requires policy enabled; issue exact head_repeat (and extension at cap), or use a new commit. A policy setting the feature false is rejected before installation; no promotion of old PASS. |
| Two central claim transactions introduce lane X with different parents | Global lock serializes them: first valid parent commits, second sees it and returns claim_conflict. |
| PASS header then LF then body containing U+00A0/U+3000 or an embedded CR | Valid UTF-8 body accepted and hashed unchanged (unless rejected by the content guard or extra VERDICT prefix rule). |
| Header containing U+00A0 after SHA, or bare CR separating header from body | invalid_verdict. Header CRLF remains valid. |
| Policy desktop entry contains grant.write or host_client_id=mac-dev | Policy JSON Schema rejects it before runtime policy installation. |


R4 scoped claim acceptance and error ordering (Q13=a): authentication/policy permission, successful-key replay and CAS retain §4's order. For a new host claim, missing reviewer.claim.write or role builder/worker is scope_denied; any caller-added field is invalid_request. Then lock the reviewed and new jobs lexicographically under the existing global lane lock. Missing reviewed job is not_found; reviewed host mismatch (or payload.host_client_id differing from caller) is host_mismatch; target ended is job_ended; target joined is review_sealed; target importing/sealed follows migration_frozen/import_sealed. A nonactive shadow target, a review-role target, different task/tier/lanes, null/wrong review_of, correction/successor attempt, reused/non-new id or conflicting frozen tester assignment is claim_conflict. New job claim_seq=expected_seq=0 is mandatory; successful replay still precedes new-id refusal. A self-referencing new review claim or null review_of is claim_conflict before target lookup. Reuse of an existing id with stale zero CAS can first return sequence_conflict; with current CAS it is claim_conflict. First binding is immutable: a host cannot correct even its own minted review claim. End and mint a different fresh assignment instead; the target's open/cap/history is unaffected. Central reviewer claims also require same task, eligible round and parent lane, while allowing a centrally selected reviewer host. Activeness means §2's live assignment, not reviewer summary.state=active. All shadow/import accounting of review subjects remains unchanged.

Decoded evidence of every content-bearing kind is checked with handoffkeep internal/guard.Reject before record/content storage, after strict decode and before business commit (HK internal/guard/guard.go:11–39; current documents already invoke it at HK internal/store/store.go:2941–2947). A match maps to invalid_request; store no content or business record, do not echo the bytes, pattern match or path on HTTP/CLI/log/audit, and do not consume a grant. The attempt may store only the stable code and fields allowed in §2. Filter imported bytes too; do not turn a guard failure into an archival bypass. For a rejected legacy import, desk records only source locator/digest, stable invalid_request and migration_history_incomplete in the migration manifest (no rejected bytes); rejected events are not counted as accepted import records. D2 blocks seal/activation pending central resolution rather than silently omitting that event or claiming a complete archive. The existing server manifest/count/accounting rules are unchanged.

R4 normative vectors (independent fixtures; every rejection leaves business records, task binding and grants unchanged):

| Input/history | Exact result |
|---|---|
| desktop mints fresh tester/reviewer/counsel claim for its active builder, exact derived fields, same task and next round | 201 claim for each allowed role; target used/next_round_no/tail unchanged; reviewer is a live assignment immediately, with no activate/grant; target open later charges exactly one slot |
| desktop mints reviewer claim for a mac-dev job | 403 host_mismatch |
| desktop mints reviewer claim for its ended job | 409 job_ended |
| desktop mints reviewer claim with different task | 409 claim_conflict |
| Host requests role builder/worker through scoped mint | 403 scope_denied; no root/builder/worker claim or task binding |
| Host changes parent_lane, owner_lane or tier from derived values | 409 claim_conflict for each; no graph edge or reparenting |
| Host supplies another host_client_id | 403 host_mismatch |
| Host supplies grant/grant_seq/extra or unknown field | 400 invalid_request; no grant or cap effect |
| Host supplies null review_of, self-reference or reviewer-as-target | 409 claim_conflict; no recursive assignment budget |
| Host reuses an existing job id with current CAS/fresh key, or requests supersedes/predecessor | 409 claim_conflict; stale zero CAS gives sequence_conflict first; successful original-key replay remains 200 |
| Host tries own minted review-role job's activate/round_open/void/joined | 403 scope_denied, including centrally attempted review-role activate; no extra budget or round |
| Target joined, target shadow, nonexistent target, conflicting tester on an existing open | review_sealed, claim_conflict, not_found, claim_conflict respectively |
| Same-host scoped assignment and centrally assigned cross-host assignment each provide matching T2/T3 PASS | Both satisfy Q9 if live at target open/verdict acceptance; host cannot mint the cross-host assignment |
| Same exact successful mint key/body replayed after target end | 200 original claim/summary, no new claim; permission is still rechecked |
| Canonical policy head_repeat_enabled=false | Policy schema validation fails before installation; installed policy requires true |
| at-integrator/pb-integrator/tt-integrator reads capabilities after A1 | Exact distinct authenticated client_id; reads allowed, grant/claim mint returns scope_denied |
| pa-integrator issues extension for a ParkingArcade job | scope_denied; only director-1/operator-desk may issue it under Q1=a |
| Verdict body contains a generated token-shaped string that guard.Match recognizes | 400 invalid_request; no content, business record or echo; audit has only allowed metadata/code. Construct a synthetic fixture at runtime, never copy a real token into a fixture/report |
| Durable commits, restore drill, generation fence or director-held receipts are unverified | D1/D2 gate refuses P2; remain P1/O2, no protection claim |


R4 review-lifecycle and binding regression vectors (receipt_required is evaluated from canonical claim, not a local mode flag):

| History/request | Exact result |
|---|---|
| Active subject A; host-minted R whose own summary.state=shadow; R done gets 5xx or unavailable server | exit 75, original intent pending; zero local job.completed, terminal relay, housekeeping or quota release |
| Same R; legacy receipt-less job.completed/job.joined/end relay | wrk_receipt_required; no protected terminal publication/counting/release |
| Same R; server accepts canonical completed, later canonical end | Each accepted receipt authorizes only R's own eligible cleanup; end makes R unavailable for new target verdict; target used and PASS history unchanged |
| Subject A joins/ends after R was minted, then R cleanup runs | R remains receipt-required; no downgrade to O2, no receipt-less cleanup; earlier accepted PASS stays valid |
| P1 shadow A; host tries scoped mint | claim_conflict, no fallback; C1 instead requires a central P1 assignment before starting the reviewer |
| P1 shadow A; central reviewer R claim then target shadow open receipt | R can start without activate; R's own done/end are fail-closed canonical lifecycle, target verdict still shadow/untrusted for future activation |
| Central tool presents R end receipt to drop A's task, both have same task_id | wrk_receipt_required; require the current bound subject A/current-claim end receipt; no task change |
| Same required-mode reviewer R tries import_begin/import/import_seal | scope_denied; never become an unprotected shadow-import job |

## 6. Client cutover (wrk)

Current local ordering and semantics: round open checks local end/head/grant then writes under the local events lock (AS bin/wrk:5826–5838,5841–5878,5881–5938); void checks grant and refuses verdict-bearing rounds (AS bin/wrk:5988–6054). joined checks the local end and join scan unless a syntactically acceptable waiver ref is supplied, uploads and writes/relays the local event (AS bin/wrk:5358–5399). The present waiver ref alone skips the local scan; O3 must validate a typed server grant instead. done is warning-only for builder round issues, resolves/uploads report snapshots and settles completion (AS bin/wrk:5122–5156,5191–5234). Delegated panewire paths exist (AS bin/wrk:4956–4960,5373–5377) and must use the same adapter or be disabled for enforced jobs; they are not a fallback around O3.

One shared client adapter provides capabilities, journal append/read, exact evidence bytes, idempotency persistence and receipt projection. The adapter executes only §6b CLI subcommands; it neither reads tokens nor implements HTTP. For a fresh enforced job the mandatory order is central claim commit, central activate acknowledgement with zero-work baseline, host GET verifying active canonical subject claim, local arbiter claim, then pane start (review-role assignment exception below). This prevents normal host work from racing fresh activation. In shadow phase only, the explicit shadow claim is followed by host execution without activation and remains O2. A host claim command never starts an enforced builder/worker pane from a shadow/importing/sealed claim. A review-role pane instead requires its live canonical assignment plus the active target and its accepted round_open. A centralized director spawn path mints the claim, fixing job/host/lane/task; hosts cannot ask the server to approve arbitrary parent data. Reviewer spawn order is: GET the active reviewed builder and its next/open round; reserve a new reviewer job id; same-host authenticated host appends the exact scoped claim using reviewer.claim.write, or central role mints a cross-host assignment; GET and verify the live assignment, same task and exact routing; append target round_open naming it (with any required central grants); only after the open receipt may local arbiter projection and the reviewer pane start. A review assignment needs no separate activate; no host step can mint a builder/root claim. If the target round is already open with tester_job_id=null, minting alone cannot change it; a matching assignment may attach at verdict as §5 permits, and no second pane starts for a conflicting frozen assignment. The builder subject always follows the central claim -> activate -> GET -> local claim -> pane sequence above. During enforcement, the legacy task link mutation in spawn is replaced by that transaction. Failure after the server claim but before local spawn leaves a visible unstarted claim; director can end it, never erase/reissue its id.

P1 tester spawn is explicit: because host reviewer.claim.write requires an active subject, C1 never attempts that mint against a shadow subject. The central role precreates the P1 tester/reviewer/counsel claim using claim.write/reviewer.claim.write, same task/derived lanes/eligible round, and may target a shadow builder/worker only during P1. C1 GET verifies that canonical assignment and the shadow subject; the target's shadow round_open receipt precedes reviewer pane start. That centrally minted reviewer already has receipt-required own completed/end lifecycle under §2; no central activate is added. The subject's shadow verdict remains untrusted and cannot satisfy a later activated join. Missing central P1 assignment pauses that tester before pane start with claim_conflict; it is not an attempted host mint followed by a 409 fallback. This temporary P1 placement step is explicit staging cost; P2/P3 same-host testers use Q13 without it. The ordinary shadow target may still use the documented 5xx warning branch, but no reviewer pane may start without its accepted assignment/open, and no reviewer completion/end may use that branch.

Dual-write, per command:

1. Capture local input bytes once; write a durable local intent with UUID key/body outside events. Execute local validation.
2. Attempt server append/read against the precreated shadow claim before writing today's event. Record server receipt on success. For an unactivated builder/worker subject in explicit P1 shadow only, an unavailable server allows local event plus one explicit job.o3_warning (code, key and operation only) and leaves intent pending. This is O2-only and cannot count as an O3 receipt. Reconciliation resubmits the same original body/key; it never reconstructs trusted facts from edited event files.
3. Write today's job.round/job.verdict/job.round_void/job.joined/job.completed event as a projection, with o3_mode, wrk_receipt and original key when available. If server acknowledgement succeeded but the local write failed, the server wins and the command reports projection_pending; a later repair reads the server, rather than submitting a new operation. Relay success is independent of authority.
4. Whenever receipt_required is true (subject activated OR canonical review-role claim, even in P1), remove the shadow fail-open branch: server append and receipt must commit before any successful local event, relay, housekeeping or quota release. A local intent is neither a round open nor a verdict. Read repair uses server records and marks provenance. Never import a local projection back as new authority.

Enforced/mixed/offline command matrix (the policy applies to the real command and delegated implementations). In this matrix enforced means §2 receipt_required, including a review-role job whose summary.state is shadow, not just an activated subject:

| Command | New wrk + new enforced server | Old wrk + new server | New wrk + old server or namespace missing | Offline/5xx for enforced jobs |
|---|---|---|---|---|
| claim/spawn | GET precreated canonical claim, verify task/host/lanes, then local claim/pane. Central role creates server claim first. | Local claim alone is unaccepted; server protected-task mutation denied; director refuses absent receipt | Refuse with O3 unsupported, no pane/claim. Explicit legacy mode only for never-enrolled O2 jobs during rollout | Fail closed before new pane/work; unstarted central claim remains visible |
| round open | POST round_open with expected_seq and exact head; receipt then local event, then reviewer starts | Local file cannot open a server round; director ignores it; legacy protected completion denied | Refuse, no local authoritative event | Fail closed; do not start another tester; retain/retry same intent/key |
| round verdict | Read bytes once; POST explicit round_no/head/bytes/digest; receipt then projection | Local verdict does not become trusted; server round remains pending | Refuse; preserve reviewer file for later | Fail closed for acceptance; preserve bytes and pending intent, no accepted verdict |
| round void | POST void referencing exact server grant; receipt then local event | Local void never reduces server count | Refuse | Fail closed; count stays charged |
| round status/audit | GET summary/full paginated snapshot; additionally scan local unmatched evidence as warning | Local result is labelled O2/unverified by director; not accepted as canonical status | May print local status only with explicit O2 unsupported warning | May print cached snapshot with stale=true, generation/seq/time and recorded warning; never report it current or use it for decisions |
| joined | Upload bytes in journal POST, server gates and creates canonical receipt/relay atomically; local projection afterwards | Protected relay/task join denied without canonical receipt; local event cannot authorize join | Refuse | Fail closed; no joined success or quota release |
| done | POST completed observation, warn if rounds incomplete, then local projection/housekeeping; completion is never join | Protected completed relay is denied without receipt; no quota release from local done | Report may be saved locally with warning, command exits temporary-failure; no accepted completion | Preserve report and write pending warning; return temporary-failure (exit 75), no OK completion or quota release |

Server round_cap maps to existing wrk exit 78; unavailable/indeterminate writes map to 75. Other results use the exact §6b mapping; the adapter never replaces a stable server refusal with local success. Cached status can exit 0 only with an explicit stale/O2 label; callers that require current status must demand a canonical receipt/read and reject that label. A 401/403 or business 409 never triggers local fallback, even in shadow: it is a configuration/refusal error. A 5xx after unknown commit outcome is reconciled by retrying the identical key/body or reading records; never invent a new key merely to bypass uncertainty.

A director waiver is a typed join_waiver grant for precisely one zero-round job/head. It does not authorize disconnected writes, lower cap, suppress an unresolved BLOCKER, or make a stale status current. During an outage, local investigation/report writing may continue with a durable warning; authority-bearing transitions wait for recovery. An operator who needs an O2-only exception must make a separate explicit rollout decision through desk, recorded as an acknowledged loss of bound, not a wrk argument that a builder can set. No offline waiver is accepted by this API.

In-flight backfill happens strictly between review rounds: drain or explicitly resolve a current live round before freeze; do not activate while a reviewer is running. Director prepares a batch of distinct per-job/per-next-round extension grants for imported jobs (one typed grant per job, never a wildcard or multi-extra). If historical verdicts reserve the chosen head, prepare the matching per-reviewer head_repeat grants as well. Commit those grants after activate, when the active claim and next round are known, and before any fresh reviewer starts. D2 owns the scheduled batch and tracks every receipt; a missing grant pauses only that job. Then director appends import_begin to freeze host writes, desk takes a bounded snapshot, archives existing event bytes as import records (only central role can submit them), records source locators/hashes and asserted original identities/times, and retains the original files. It then appends import_seal over the exact manifest before activate; missing/mismatched seals fail import_unsealed. Each archived event becomes an import journal record with legacy_kind, including claims/rounds/verdicts/voids/joined/grants when present; legacy claimed attribution is never authenticated attribution. The canonical claim comes from director-owned assignment evidence and explicit central judgement, not from the builder's latest event or mutable task refs alone. Imported voids/grants/joins never affect authority. Missing history and planted pre-import events cannot earn trust: activation uses a floor of at least the whole base cap, preserves negative path/content/head identities conservatively, and requires a fresh server-held review plus extension if completion needs it. Director can leave a job O2-only until it ends; no automatic positive trust promotion exists. Already completed legacy jobs are archived for observation and do not become O3 completions retrospectively.

## 6a. Receipt consumer inventory (C2 and K1)

Every source citation here resolves to AS, AS242, HK or PW in the header; repository names are spelled out in the table. These are code entry points unless marked process. C2/hk 1927 owns exhaustive wiring and integration acceptance; S1/hk 1925 implements the handoffkeep service/store fence and C1/hk 1926 supplies the CLI adapter used by these consumers. K1/hk 1929 owns documented gates. A receipt must resolve through a fresh canonical read to the exact immutable job, seq, kind, record_hash, store_generation, claim/task and evidence digest; joined additionally binds the PR/head and all canonical verdicts. Read full paginated history where a decision depends on it, verify content/hash continuity, and reject history_regressed. A local wrk_receipt object or local schema-shaped event alone is insufficient. §2 receipt_required includes canonical review-role jobs even when their summary.state is shadow; all entries below must apply that predicate to reviewers as well as activated subjects. Unknown or unreadable jobs fail closed for completion/merge/quota decisions; server enrollment cannot be inferred from a deletable local marker.

Canonical completed authorizes only accepted report observation and associated completion cleanup; it is not a PASS or merge. Canonical joined authorizes the join candidate after the existing Git/CI/family gates; it is not a merge command. Canonical end authorizes final cleanup. Release-time consumers recheck the matching canonical terminal/observation and current assignment immediately before releasing each held reservation. No new work can acquire/revive an ended job id. Claims, telemetry and notification delivery cannot stand in for those records.

| Repository, pinned file:function or documented process | Required change | Owner |
|---|---|---|
| agent-skills AS bin/wrk:5191 done_cmd, :4607 completion_event, :4740 note_duplicate_completion | Append completed via C1 before OK, duplicate suppression or housekeeping; compare exact report bytes/receipt even on local duplicate path. | C2 hk 1927, C1 adapter |
| agent-skills AS bin/wrk:5337 joined_cmd, :5266 builder_flat_event, :5041 wrk_join_blocks/:5092 wrk_joined_refuse_blocks | Resolve canonical joined and round/verdict chain; typed waiver only; local scan adds unmatched-file warning/refusal but never authorizes joining. | C2 hk 1927 |
| agent-skills AS bin/wrk:4969 delegate_job_cmd and :5239/:5373 delegated done/joined branches; :4828 emit_relay_event | Pass canonical receipt to the upgraded panewire implementation and verify its result; otherwise disable delegation for enrolled jobs. Never take a local or Go fallback after refusal/unknown outcome. | C2 hk 1927 |
| agent-skills AS bin/wrk:6456 job_events_scan, :6538 job_watch_current, :6581 job_end_record, :6692 quota_release_round_current, :6727 release_job_quota_pool | Local census is display/projection only for O3. Gate every release immediately before each arbiter release with fresh canonical completed/joined/end evidence for the current claim; retain existing live-sentinel/pane and stale-scan checks. No best-effort success from a missing receipt. | C2 hk 1927 |
| agent-skills AS bin/wrk:6957 job_end_housekeeping and completion sentinel paths :6940–7238 | Server acceptance precedes sentinel shutdown, terminal registry publication and quota release. Sentinel discovery of report bytes must use C1 completed; confirmed loss must append end or leave cleanup pending, not synthesize authority from local terminal JSON. | C2 hk 1927 |
| agent-skills AS bin/wrk:11129 reap_cmd, :11113 reap_event; :11529 prune_sentinel_row and :11634 prune_cmd passes 1–3 | Preserve dry-run and existing guards; successful O3 close/reap requires a canonical end/accepted completion reference before mutation and paired release. A candidate scan is not a receipt. | C2 hk 1927 |
| agent-skills AS bin/wrk:11732 prune_cmd pass 4 and :11774 ended-no-sentinel release call | Canonical recheck inside the shared release function covers the old-sentinel gap; no local ended flag, missing sentinel or arbiter row alone releases quota. Apply and dry-run describe the same canonical eligibility. | C2 hk 1927 |
| agent-skills AS bin/wrk:2971 arbiter_release_held, called by spawn failure EXIT traps :3361/:10396 | Direct release must use the shared canonical gate too. An admitted protected job gets canonical end before failed-spawn release; if no canonical job was ever admitted, only the exact locally acquired, never-started reservation from that failed spawn may be compensated, with recorded spawn identity and no completion/terminal authority. An unavailable/indeterminate claim outcome is reconciled first, never treated as absent. | C2 hk 1927 |
| agent-skills AS director/bin/quota-stale-report:57 read_events/:91 categorize/:110 main | Local completed_no_release/reaped/lost buckets remain observations. A protected cleanup candidate requires a fresh canonical completed/end and §2 receipt_required lookup, including reviewers; report cannot authorize release from the local bucket. | C2 hk 1927 |
| agent-skills AS bin/wrk:10147 hk_task_takeover_rebind and :10178 old-job release; :10385 spawn_cmd and :10258 hk_task_link | Central successor/claim transaction replaces protected legacy rebind/link; old end and new claim receipts precede release/spawn. Scoped reviewer spawn follows §6, never generic task claim. | C2 hk 1927, C1 handshake |
| agent-skills AS bin/arbiter:885 cmd_release and :645 cmd_claim; direct resource release API is a local projection | For O3, legitimate release callers require canonical evidence; direct local DB/event edits cannot make director/hub treat quota as canonically released or a task complete. Local admission alone never authorizes pane work. Preserve the explicit limitation that unmanaged local execution cost is unbounded (§1). | C2 hk 1927 |
| agent-skills AS director/merge_precheck.py:134 parse_report, :240 check_reports, :465 check_hash, :494 evaluate, :586 gather_live, :684 audit | Fetch immutable server verdict/report and current claim/receipt, bind tested head/PR/task and store generation; local report hashes supplement canonical evidence. Persist director receipt/checkpoints and refuse stale/unknown/regressed data before merge readiness. Existing G3 CI/H/B/M checks remain. | C2 hk 1927 |
| agent-skills AS director/bin/tester-eligible:189 _report, :1401 _job_record, :1457 contributor_observations, :1491 evaluate, :1703 audit | Resolve canonical claim/reviewer assignment and accepted verdict; local job.spawned/pane aliases cannot establish authority or substitute for server content. Preserve family/effort observations as separate evidence. | C2 hk 1927 |
| panewire PW job.go:112 jobCLI.done, :239 jobCLI.joined, :660 completionEvent, :740 builderFlatEvent, :895 emit; job_close.go:104 close/:265 publishJobClose | Delegated CLI obeys the same C1 contract, receipt-before-event/cleanup and typed join gate; disable O3 mutation until upgraded. Close/revoke cleanup needs canonical end. No standalone Go completion bypass. | C2 hk 1927 |
| panewire PW emit.go:112 runEmitCLI/:239 writeEmitRecord; daemon.go:745 emitRelayEvent; hub_jobs_client.go:188 scanHubRelayEventsWithin/:356 scanHubJobCompletion/:472 scanHubJobEventDetails | Carry receipt across local emit, watcher/scanner and node outbox; receipt-less terminal events may warn but cannot publish completion, remove active work from canonical counts or create terminal job state. Replay revalidates, not just first delivery. | C2 hk 1927 |
| panewire PW hub_client.go:1793 relayEventWireForm/:1865 relayEventForSend; hub.go:1150 handleAgentMessage/:1403 handleRelayAck; relay.go:432 relayJobEventFrom/:866 persistRelayEventRecord | Verify journal job.completed/job.joined before persistent publication, routing, replay or acknowledgement; preserve receipt on every wire shape. Server must atomically project canonical completion, so hub uses exact dedup/retrieval rather than creating authority from a flat local event. | C2 hk 1927 |
| panewire PW relay.go:498 reacknowledgeRelayEvent/:894 markRelayEventDelivered/:947 recordLateRelayDelivery/:1230 replayRelayEvent; relay_dedupe.go:89 relayDeliveredIdentity | Revalidate the canonical receipt for protected terminal rows on duplicate/late/replayed delivery paths. Existing event-id/payload delivery identity remains necessary, never sufficient for completion authority. | C2 hk 1927 |
| panewire PW idle_wake.go:253 Observe/:586 tick/:671 materializeAssigned/:711 resolveIdleWakeOwner | Idle/done is a harvest wake only, not completion evidence. Resolve job/owner from canonical claims for O3 and fetch canonical completion before counting completed work or cleanup. Legitimate idle-wake lane.event needs no fabricated completed receipt and must remain explicitly non-authoritative. | C2 hk 1927 |
| panewire PW busy_relay.go:828 deliver/:1039 reconcileDeliveredHeld; relay_delivered_store.go:29 RecordRelayDelivered/:47 RelayDeliveredByKey; relay.go:894 markRelayEventDelivered | relay_delivered counts delivery only. Any completed/joined tally or quota/removal projection fed by it requires a verified canonical terminal receipt. Preserve existing notification/dedup counts for ordinary lane/idle-wake messages without calling them completed work. | C2 hk 1927 |
| panewire PW fleet_census.go:467 scanFleetCensusJobs/:639 fleetCensusJobVerdict/:903 buildFleetCensusResult | Distinguish local observations from canonical completion/end when presenting cleanup eligibility; no downstream automated release from a local terminal label. | C2 hk 1927 |
| handoffkeep HK internal/api/api.go:249 CreateTask/:253 ClaimTask/:257 RebindTaskJob/:261 NextTask/:274 TransitionTask/:277 RelaneTask/:280 SetTaskProject; HTTP handlers :679, :842, :869, :899, :922, :955, :1086 | Pass authenticated principal and receipt through one protected-task fence. Claim/rebind/ref/project/relane mutations are denied; unrelated legacy operations retain behaviour. State-only protected changes require central principal and matching canonical claim/joined/end. tasks next excludes protected tasks. | C2 hk 1927 inventory; S1 hk 1925 fence |
| handoffkeep HK internal/store/store.go:1730 CreateTask/:1793 claimTaskTx/:1880 ClaimTask/:1896 RebindTaskJob/:1967 NextTask/:1991 TransitionTask/:2148 RelaneTask/:2202 SetTaskProject | Enforce inside transactional storage boundary too, so direct service/UI/MCP/store calls and no_job cannot bypass; canonical claim is the only protected enrollment/rebind writer. Do not trust free by or refs.job_id. | C2 hk 1927; S1 implementation |
| handoffkeep HK internal/api/api.go:82 Service.ResolveDecision/:1012 taskDecisionResolve/:1200 decisionResolve; internal/store/decision_request.go:682 ResolveDecisionRequest/:694 ResolveDecisionRequestAssistant/:709 resolveDecisionRequest; internal/store/disposition.go:506 ApplyDisposition | Receipt-gate protected decision resumption/terminal mutation, including resolve->claimed and disposition actions. Binding changes use the canonical claim transaction only; merely recording an answer grants no completion authority. | C2 hk 1927; S1 implementation |
| handoffkeep HK internal/api/assistant.go:89 assistantDecisionResolve, internal/api/api.go:1174 dispositionApply; internal/mcp/server.go:39 newServer/:196 HTTPHandler | Current assistant route calls Store directly: move/invoke the same principal/receipt fence. MCP currently registers generic document/memory/attachment tools, not task mutation tools; keep generic writes non-authoritative, fence future task tools, and never promote a stdio client string or UI session to operator-desk. | C2 hk 1927; S1 implementation |
| handoffkeep HK internal/api/api.go:320 AppendRelayEvent/:323 MarkRelayEventDelivered/:1222 relayEventsCreate; internal/store/store.go:2516 AppendRelayEvent/:2545 MarkRelayEventDelivered | Require canonical receipt plus exact job/kind/head/report digest before storing or counting a protected completion; atomic journal relay projection wins over legacy insertion. Delivered status cannot manufacture a missing canonical completion. | C2 hk 1927; S1 implementation |
| handoffkeep HK cmd/handoffkeep/main.go:276 tasksCmdE/:623 decisionsCmd; internal/remote/remote.go client legacy methods | Carry explicit receipts/principal through canonical CLI transport; forbid implicit by-based promotion. New wrk namespace is S1's §6b implementation. | C2 hk 1927 verifies compatibility; S1 implementation |
| agent-skills AS director/SKILL.md:40 builder placement, :238 merge gate, :266 gate 1, :276–279 post-merge cleanup; AS builder/SKILL.md:154–185,:276–294,:407–465 review-round/completion gate (process) | Before spawn verify canonical central subject claim/activation; before review start verify scoped/central assignment and round-open receipt; before READY/JOIN/merge fetch canonical exact-head PASS/joined/content and retain independent receipt. After terminal transition recheck canonical cleanup evidence. Preserve CI, family and other gates. | K1 hk 1929 |
| agent-skills AS checker/SKILL.md:24–37 G1/G8 and :108 inbox harvest (process) | READY requires canonical joined/PASS/content reads before acting on a local joined/completed relay. State change uses central authenticated subject-bound receipt; CHECKER_LANE/by alone is not task.state authority. Inbox wake asks for harvest, never proves completion. | K1 hk 1929 |
| agent-skills AS orchestrate/SKILL.md:99–102 cleanup; spawn-worker/SKILL.md:486–595 spawn/landing and :1044–1128 cleanup (process) | Replace direct arbiter terminal release with shared receipt gate, preserve unstarted reservation compensation rule above; include §6 Q13/P1 assignment/open-before-pane order. | K1 hk 1929 |
| agent-skills AS director/panewire-events.md:26–35 completion/join/idle/claim signal table (process) | Mark terminal delivery as a receipt-verified observation and idle as harvest only; include canonical reviewer lifecycle and missing-signal handling, no implied local-event authority. | K1 hk 1929 |
| Private PLAYBOOK gate/deploy checklist (process; reference at agent-skills AS director/SKILL.md:10–12,:284–289, not a repository code file) | Add canonical receipt lookup/retention to review harvest, merge readiness, task closure and quota/pane cleanup; P2 checklist requires full C2 inventory, PR242 reconciliation, between-round activation, grant batching, Q1 hand-off and Q12 desk restore drill. K1 locates the desk-owned live PLAYBOOK and records its document/version during implementation; this revision does not claim to have read private PLAYBOOK contents. | K1 hk 1929, desk maintains ops checklist |

C2 acceptance re-enumerates all terminal publishers, protected task mutators, quota releasers (including failure compensation) and delivery/idle consumers at its actual implementation heads, and reconciles each new site with this table. This includes negative tests planting a local completed/joined/end, deleting local enrollment, faking a receipt hash, replaying delivery, invoking delegated commands, assistant direct-store resumption and prune pass 4. All must fail to change canonical completion/merge/release without the required record; an idle wake still occurs as a non-authoritative harvest prompt. K1 likewise re-enumerates every operational skill/checklist that can produce READY, task terminal or direct release, including checker/orchestrate/spawn-worker/panewire-events above; it records each actual document/version. This is required work, not an optional future audit.

## 6b. Canonical handoffkeep CLI transport (S1 supplies, C1 calls)

The only network client for wrk journal operations is the new handoffkeep wrk namespace implemented by S1/hk 1925. C1/hk 1926 executes it by argv with no shell evaluation, through the PR 242 guard-selected canonical binary. In production that is the OS-account home plus .local/bin/handoffkeep, resolved and checked as AS242 bin/wrk:4947–5005 specifies; never PATH lookup, caller-provided HANDOFFKEEP_BIN, the unguarded hk_bin helper, a job-local executable, or a cached path accepted without the guard. Validate before capabilities, every append and every read/content/pagination call. Keep the O2 guard for every remaining local round/grant/takeover read not replaced by this adapter. A noncanonical override is a trust-path refusal, exit 2, before exec or network. Test-only overrides require WRK_TEST_MODE=1 and all paths under temporary fixture roots as PR 242 specifies; no production fallback uses that exception.

handoffkeep owns its installed authenticated HTTP transport and credential loading. wrk never reads a token/config credential file, sends an Authorization header, implements HTTP, accepts an env token as an alternate transport, or falls back to curl. No new CLI flag can select a token, token path, server URL or transport; the wrk namespace uses the existing installed handoffkeep remote configuration and redirect protections (HK cmd/handoffkeep/main.go:199–216; HK internal/remote/remote.go:117–141). C1 removes HANDOFFKEEP_URL, HANDOFFKEEP_TOKEN, HANDOFFKEEP_CF_ACCESS_CLIENT_ID and HANDOFFKEEP_CF_ACCESS_CLIENT_SECRET from the child environment by key before every canonical CLI invocation (do not read/log their values), and supplies the guard-resolved account home. The existing CLI config loader then uses installed configuration for that child; it cannot inherit a caller-selected endpoint/token or CF identity. Direct authorized central-role CLI wrappers outside C1 retain their existing authentication configuration; this is not a new credential scope or an override flag. This does not claim same-user installed-config isolation. Director/consumers independently fetch canonical receipts through their own installed configuration. S1 overrides remote.Client CheckRedirect for this namespace to reject EVERY redirect, including same-host redirects that the existing remote transport permits; never follow Location or resend evidence. No endpoint or credential material appears in errors.

All commands accept exactly the listed arguments; unknown/duplicate flags and extra positional arguments fail exit 2. Integers are safe nonnegative decimal integers (SEQ positive); UUID/head/job/kind values obey §5. A read command receives no stdin payload; nonempty supplied stdin is invalid_request. append reads one strict UTF-8 JSON object through EOF with no duplicate keys, comments, trailing data or unknown fields, and validates the applicable Input_KIND schema before constructing the request. The request-size/evidence limits in §5 still apply. Stdout on success is exactly one newline-terminated JSON object, unwrapped and conforming to the named §5 schema; diagnostics/progress/HTTP headers never go to stdout. Exit 0 means valid 200/201 and validated contract/response, not merely process completion. It never means a pending write succeeded.

| Subcommand and exact arguments after handoffkeep | Stdin | Request / stdout JSON |
|---|---|---|
| wrk capabilities | Empty | GET /v1/wrk/capabilities -> Capabilities |
| wrk append JOB --kind K --idempotency-key U --expected-seq N --claim-seq C | Exact Input_K payload object, including content_b64/report_b64 when K needs it | POST /v1/wrk/jobs/JOB/records, Idempotency-Key U, body {kind:K,claim_seq:C,expected_seq:N,payload:stdin-object} -> WriteResponse. 201 is created=true; successful replay 200 is created=false. No implicit GET or seq/key rewrite. |
| wrk job JOB | Empty | GET /v1/wrk/jobs/JOB -> JobResponse |
| wrk records JOB [--after-seq N] [--snapshot-seq S] [--limit L] | Empty | GET records with these exact numeric query keys; omitted after_seq=0, limit=100, snapshot chosen by server -> RecordsResponse. The returned pair snapshot_seq,next_after_seq is the cursor for the next call; C1 must keep snapshot fixed. No automatic partial-success pagination. |
| wrk content JOB SEQ | Empty | GET /v1/wrk/jobs/JOB/records/SEQ/content -> ContentResponse, still JSON with exact base64, not decoded terminal text |

On every failure stdout is empty and stderr is exactly one newline-terminated line: handoffkeep wrk: CODE. CODE is a stable server Error.error from the table below, or a listed local transport/validation code. Never print server body, URL, paths, submitted bytes, HTTP error prose or a raw exception. The stdout response is buffered and validated completely before publication. One stable code always maps to one process exit; multiple codes may share an exit as shown, so callers use both exit and exact code. The server's HTTP/status/code map in §4 is unchanged. wrk surfaces the same code/exit (with its own single prefix if needed), except status/audit may explicitly display a stale read as §6 permits.

| Stable code(s) | CLI/adapter exit |
|---|---|
| round_cap | 78 |
| wrk_unavailable, wrk_recovery_required | 75 |
| unauthorized, scope_denied, host_mismatch | 77 |
| not_found | 66 |
| invalid_request, invalid_cursor, invalid_verdict, invalid_path, grant_before_cap, request_too_large, method_not_allowed, unsupported_media_type | 65 |
| idempotency_conflict, sequence_conflict, claim_conflict, job_ended, review_sealed, round_not_open, round_has_verdict, round_voided, head_mismatch, duplicate_path, duplicate_content, duplicate_head, grant_invalid, grant_consumed, grant_expired, zero_rounds, review_incomplete, blocker_unresolved, wrk_receipt_required, import_unsealed, import_sealed, migration_frozen | 73 |
| cli_usage (local argument error, before sending) | 2 |
| transport_unavailable, transport_indeterminate (local network failure/timeout; the latter may have committed), protocol_invalid (malformed/unknown status/code/response, redirect, unsupported contract/old-server namespace), o3_unsupported | 75 |
| history_regressed (local retained-receipt comparison) | 75 |
| intent_invalid, intent_io (local durable intent validation/storage failure; no send if intent was not durably saved) | 74 |

Local strict payload validation failure uses invalid_request/65; argument-shape errors use cli_usage/2. A valid authenticated O3 job read missing that particular job is not_found/66; a missing namespace, non-JSON 404 or unsupported contract is o3_unsupported/protocol_invalid 75, with no local-event fallback. CLI makes at most one network attempt per invocation; retry scheduling belongs to C1 and preserves the original request. A known sequence_conflict means no success was committed for that key; C1 may refresh, validate the intended operation again, then create a new intent/key. An unknown outcome must first retry the original exact key/body, even if the current server seq/claim changed. No mutation is silently retried with refreshed CAS or a fresh key.

Durable intent path: canonical job directory /o3-intents.json, outside events, regular file mode 0600, no symlink; sibling lock /o3-intents.lock serializes local writers. The containing job directory is the PR242-selected canonical directory, not a caller-relative path. A review-claim mint intent belongs in the NEW REVIEWER job's canonical directory; creating that local directory/intents is not a local arbiter claim, quota admission or pane start. Central/host mint and later retries use that same job-id namespace. This operational persistence is not an authority boundary. The file is {version:1,intents:[...]}; every intent has local_id (UUID), job_id, idempotency_key (UUID), the entire immutable Request object, request_sha256 as §5 defines, state pending/accepted/rejected, receipt null or the exact legacy receipt tuple, error null or stable code. Store exact evidence as its canonical base64 inside Request, so a changed reviewer file cannot change a retry. No credential data is stored. Accepted requests are retained for read repair/audit; do not reconstruct intents from events. Multiple sequential/concurrent pending operations have distinct keys; lock+atomic write/rename+file and directory fsync happens before sending and before recording a terminal intent state. Refuse on persistence failure; never send an unsaved request.

For append, every response that fails the expected contract/status/schema validation (protocol_invalid/o3_unsupported), every non-contract 5xx, and even a recognized 503 is an indeterminate write outcome. Leave the intent pending and retry its original key/body; do not mark it rejected, change CAS or allocate a new key. Only a completely validated 4xx Error response constitutes a definite server rejection; a parsed HTML/error string or an unexpected 2xx does not. This uncertainty rule is distinct from the common exit 75. After a lost response or crash, reopen the same pending entry, verify its request digest and exact fields, and resend its original key/body including expected_seq and claim_seq. A successful replay returns the same committed record and original summary; fetch a current job before any next decision. Store the receipt durably, then project local events and perform eligible cleanup. If server acceptance was known but receipt persistence failed, leave the original pending intent and retry it; do not publish success. If a server reply is a definite rejection, record its stable code without consuming the idempotency key; a later intentional refresh creates a new entry. The server remains the authority if this file is edited or deleted; consumers never accept the file as a receipt without canonical verification.

| Transport/intents vector | Required observation |
|---|---|
| PATH contains a fake handoffkeep; HANDOFFKEEP_BIN selects a different executable; replaced canonical binary is group/world writable or foreign-owned | PR242 guard refuses the invalid override/binary before exec (exit 2); fake tool sees zero calls. An innocuous fake PATH entry alone is ignored and canonical CLI runs. |
| Capability/job/records/content call attempts an unguarded hk_bin lookup | C1 test spy rejects it; every operation uses the same canonical resolver, including later cursor pages. |
| Token present only in caller environment while canonical CLI is unavailable | exit 75 or trust-path exit 2 as applicable; no HTTP/curl/env-token fallback and no local completion/quota success. |
| Server returns {error:host_mismatch}, {error:round_cap}, {error:sequence_conflict} | Empty stdout; exact one-line stderr code; exits 77,78,73 respectively; no local fallback. |
| Server commits append, response is lost, reviewer file subsequently changes | Original pending base64/body/key/expected_seq is resent; 200 created=false with identical record/hash; exactly one charge, changed file ignored. |
| Definitive sequence_conflict, then refresh | Keep rejected original intent, create a new key only after revalidation; do not change original immutable request. |
| Crash before send / after commit / after accepted-intent fsync but before projection | Saved intent retries once by key / successful replay / GET-based projection repair; no lost committed round, new key or double release. |
| Intent is symlink, mode is not 0600, malformed JSON/digest or fsync fails | intent_invalid/intent_io exit 74 before send; no event or accepted completion. |
| Cursor page has different snapshot, missing seq, altered hash or generation | protocol_invalid/history_regressed exit 75; no decision based on partial pages. |
| CLI gets redirect, HTML response, missing namespace or unknown stable code | protocol_invalid/o3_unsupported exit 75; one safe line, empty stdout, no body/Location/evidence echo. On append keep original intent pending and exact-key retry. |
| CLI available, but caller env endpoint/token/CF settings differ from installed config | C1 child has all four override keys absent, no values read/echoed; canonical CLI uses installed identity/endpoint; no request reaches the caller-selected fake endpoint. |
| Append commits but proxy returns HTML/non-contract 5xx or malformed success | protocol_invalid exit 75, pending intent unchanged; retry original key/body returns accepted replay, exactly one record/charge. Recognized 503 also stays pending. |


## 7. Migration and rollout

No deploy is performed by this design task. Current task/document/relay code has no /v1/wrk routes (HK internal/api/api.go:417–480). New data is additive; legacy schema must not be removed. PR 242 is still pending at AS242; C1/D2 re-read its eventual merged head and preserve its narrowed controls during rollout. The pinned current-code inventory below is a starting checklist, not permission to miss a newly added route.

| Phase and controlling flag | Entry/check | Rollback |
|---|---|---|
| P0 schema/auth policy: server WRK_RECORDS_API=off | Install additive tables and policy parser; old auth grammar and existing endpoints remain compatible. No enrollment. Desk tests policy fixtures and duplicate-token/id rejection without exposing values. | Disable API, keep new tables; previous binary safe because no enforced jobs exist. |
| P1 shadow: server WRK_RECORDS_API=shadow, wrk --records-mode=shadow | Centrally create shadow subject claims and P1 review assignments as §6 specifies. Subjects dual-write with warnings; review-role own lifecycles are receipt-required from claim under §2. Compare counts/identities/parent/coverage. Director still calls the subject/fleet bound O2 until Q12/P2. | End any live canonical reviewer assignments through the server before stopping their transport; keep receipt fences for them. Subject shadow clients may return to O2; preserve records/projections, imports stay untrusted. No subject/fleet bound was claimed. |
| P2 host canary enforcement: server WRK_RECORDS_API=enforce, per-job activate record, client --records-mode=required | First desktop canary, then mac-dev, mac-work, rpi. Q12=a makes durable commits with synchronous_commit=on, desk backup/WAL restore drill, store-generation freeze fence and independently retained director receipts mandatory preconditions before P2. Activate only between rounds and after server, wrk, CLI, panewire/delegation, director gates and every quota consumer support receipts. Subject activation and canonical review-role enrollment are authoritative under §2; flags cannot downgrade either. | Pause enrolled jobs. Run earlier O3-capable binary/read-only mode if necessary. Keep protected-task/receipt gates active; finish or end through recovered server. Do not roll back to a pre-O3 server while any enforced job is active. |
| P3 fleet default: central claim policy NEW_JOBS_RECORDS_REQUIRED=true | All new builder/worker subjects receive central claims and activation; review-role assignments follow Q13; imported in-flight jobs use conservative floor. Nonreceipt completions and unknown jobs are refused by director/quota consumers. | Stop new enrollment; P2 rollback for active jobs. Legacy-only jobs remain explicitly marked O2. |
| P4 mbp: same flags, host client mac-personal | Verify functionality and attribution while documenting the shared-user limitation. A1 installs at-integrator/pb-integrator/tt-integrator attribution before their P4 verification; no grant scope or mbp isolation claim. | Same data-preserving rollback; no claim of mbp isolation before or after. |

Flags are deployment policy, not authorization from caller environment. A client mode selects a transport behaviour, never overrides the server's activated job. Disabling the API after enrollment must return wrk_unavailable while legacy fences remain active. An older process that would remove fences must be prevented by deployment checks. O2 trust-path hardening remains needed on every host for local discipline; on mbp grant/claim authority remains only narrowed. No credential file is copied to a new host as part of this design.

Desk-run per-host verification, using client ids only and disposable canary jobs/tasks: authenticate through the installed wrapper and inspect capabilities.client_id; central role mints a canary claim for that host; verify its round open and server-computed verdict digest; wrong-host submit denied; grant write under host token denied; centrally issue one extension, consume once and prove a second consumption denied; verify locally edited projection cannot change server count; simulate a transport interruption using test tooling and verify no successful joined/done/quota-release without receipt; verify exact-key retry, terminal refusal, pagination and central read of the same tail hash. Test mbp separately through mac-personal, pa-integrator, director-1 and operator-desk wrappers; at/pb/tt wrappers show at-integrator/pb-integrator/tt-integrator after A1, each with no claim/grant scope; pa-integrator grant attempts are denied, and its escalation hand-off below is tested. Desk reports only ids, expected stable codes, seq/hash and pass/fail, no secrets. A per-host verification record must be attached to deploy acceptance. At least one actual canonical job transition within 24 hours, checked by a director read, is required before a host rollout is considered complete.

Q1=a ParkingArcade hand-off, sent by K1/D2 before P2: message name O3-PA-GRANT-REQUEST. pa-integrator -> director-1 (or operator-desk if director is unavailable), fields task_id, job_id, current claim_seq, next_round_no, exact head, requested grant_type, tester_job_id when needed, latest verdict/receipt refs and reason_code. Text: ParkingArcade round cap reached; request central typed grant for the named job/round/head; no new tester starts until the committed grant and round_open receipts are verified. Only director-1/operator-desk may append the grant. The response carries canonical grant seq/hash, never an approval string that a host treats as authority. A denied or pending request pauses that job. No credential moves between lanes. This message is a specified future process contract; R4 sends no fleet notification itself.

Backup/recovery flag: WRK_RECORDS_RECOVERY_REQUIRED=true freezes new writes and all authority consumers. An operator records a fresh store_generation after a point-in-time restore, reconciles externally retained receipts, and re-enables only after no acknowledged seq/hash is missing. Never silently turn a restored database into an old-server fallback. See §8 for the residual durability limit.

## 8. Failure and abuse cases

A rejection produces only an immutable attempt audit record with the stable code, never the rejected business record. The table explicitly distinguishes accepted records from failed attempts.

| Case | Decision and record that results |
|---|---|
| Replay old verdict, identical successful request/key | 200 with original verdict record/receipt, created=false; no extra cap count. |
| Replay old verdict with fresh key/path | duplicate_path or duplicate_content audit attempt, no new verdict; if bytes/path are changed, duplicate_head blocks at round_open without an exact head_repeat grant. A fabricated new textual review is still possible with the host token, but costs a legitimately opened round and is only host-attested. |
| Verdict for a job claimed on another host | host_mismatch attempt, no record. Exception only for a centrally minted cross-host tester claim with exact review_of and host, matching round/head/task; then a verdict records the submitting host and tester job id. |
| Grant for a different job/claim/action/round/head | grant_invalid attempt. Grant remains unconsumed on its own job. A key copied from another job does not cross its namespace. |
| Two testers on one head (hk 1901) | Separate numbered rounds and distinct tester claims; second requires a one-use head_repeat grant naming the earlier round and second tester. At cap also requires extension. Both verdicts persist; neither is overwritten or voided. BLOCKER after PASS prevents join until a further authorized PASS resolves it. This is the decided Q2=a resolution of hk 1901; installed policy enables it, while a missing exact grant still gives duplicate_head. |
| Builder forges content_sha256 | Server hashes exact bytes and rejects mismatch with invalid_request attempt. If builder submits invented bytes and their correct hash, the stored verdict accurately attests those bytes and host credential, not truthful review. O3 does not establish same-host reviewer authorship. |
| Server loses a write after acknowledging | Normal protection: content/journal/grant consumption/receipt relay commit in one PostgreSQL durable transaction, synchronous_commit=on, no 2xx before Commit. Client/director retains generation/seq/hash receipt. If a client/director observes a lower seq, changed generation, or a different hash at a retained seq, it locally emits history_regressed and refuses further authority use. This is a client/consumer code, not a server response or wrk_attempt record. It requests the operator recovery freeze; the server then returns wrk_recovery_required. A regressed server cannot detect its own missing history without an external witness; no lower count is accepted by a witnessing consumer. Lost receipt with surviving write is recovered by successful-key replay. Total destruction of both server history and independent receipts cannot be detected by a journal alone and is outside the actor bound. No false claim of solving Byzantine storage. |
| Clock skew between hosts | Host time is asserted metadata only; server seq orders all operations. Grant expiry uses server UTC at consumption. Result is a normal record with server received_at; skew cannot buy more slots or resurrect a grant. A bad server clock is deployment failure; freeze grants until corrected rather than using host time. |
| Leaked desktop/mac-dev/mac-work/rpi token | Attacker/agent can submit host-attested evidence and consume valid grants for jobs already assigned to that id, and read fleet evidence. It can mint only the §2 scoped review assignment for its own active subject; it cannot mint root/builder/worker claims or grants, write another host's jobs without a central cross-host assignment, or exceed counters. Each accepted write records the leaked client id; denied operations create scope/host attempts. Operator rotates that host credential and pauses affected jobs. No proof of which pane leaked/used it. |
| Leaked director-1/operator-desk token, including shared-user mbp use | Valid privileged records can be minted within API checks. This breaks authority assumptions for the whole fleet, not just mbp jobs; revoke/rotate and audit central records. Per-host least privilege bounds builder-only host compromise, not privileged-token theft. |
| Two simultaneous grant consumers, or extension followed by void then reuse | Job lock and unique consumption constraints choose one winner; second is sequence_conflict until refresh, then grant_consumed. A void record never returns grant capacity. |
| Builder plants fake legacy events before import or erases real events | Only import records result; asserted authors remain untrusted. Activation reserves at least three used slots; no forged PASS, parent or grant is promoted. Incomplete history blocks evidentiary acceptance and produces migration warning; fresh review uses explicit extension. |
| Builder deletes all local events or selects another inbox/hk binary | Existing canonical records remain unchanged. Director rejects fake/missing receipts and reads server. Local fake OK and hidden review effort are possible; no accepted server transition results. |
| Server commits but reply/local projection is lost | Retried original key returns original record; GET repairs projections. No duplicated round/grant consumption. Local transport warning is not permission to proceed with a new tester. |
| Legacy client posts job.joined directly, or task refs are relinked | Receipt fence rejects with wrk_receipt_required; no terminal relay/task mutation. Canonical claim remains unchanged. Unknown task/job completion cannot release quota at the director. |
| End races open/verdict/grant | Serialized journal: if end wins, new mutation is job_ended; if mutation wins, its record precedes end. There is no ambiguous timestamp ordering. |

Residual availability limit: fresh review ids can collide with a future predictable central job id. This grants no authority or cap slot; the later central claim is claim_conflict. Director uses fresh unique ids and treats collisions as visible assignment conflicts; this revision does not invent a new namespace/reservation permission outside Q13.

Receipts kept only by the submitting builder are not independent witnesses. The director must fetch and retain each accepted completion receipt and periodically retain activated-job tail checkpoints under its own host/role context. Deployment backups/WAL policy and the freeze gate are required before claiming operational durability. This adds evidence of storage regression without claiming a cryptographic security boundary from another file under the same OS user.

## 9. Task ownership and delivery order

These task ids already exist; this document creates no task or deployment authorization. Grades follow the adoption review: C1 is raised to S+, A1 lowered to B/T1. Tier for code follows its enforcement surface, distinct from this design's T2. D1/D2 are desk-owned operations, not builder implementation tasks.

| Piece / real hk id | Grade / tier / owner | Scope and acceptance | Dependencies / delivery |
|---|---|---|---|
| T0 operator decision, hk 1922 | Decision / done | dr-1922-1=A, every Q1–Q13 recommendation accepted; desk commitments comments 7044/7046. The decision is done; the task queue's state was still backlog when read, and this revision does not mutate it. | Input to R4, not an unresolved choice. |
| R4 design revision, hk 1923 | S / T2 / b1923-sol | This single revised design; all six change groups and cross-section/schema consistency; independent other-family reviewer and recorded design-only rounds. | T0; publish separate -r4 key, preserve r3. |
| M1 fixtures/harness, hk 1924 | A / T1 / builder | Sanitized legacy fixtures, Draft 2020-12 bundle checks, all normative vectors, golden old/new/offline/CLI error/intents and scoped-claim cases; no policy judgement or production access. | Accepted R4; independent acceptance remains with S1/C1/C2 testers. |
| S1 authority core and CLI, hk 1925 | S+ / T3 / builder | Append-only tables/roles, policy/claim limits including Q13, grants, CAS/idempotency/hash/content, internal/guard, task enrollment, complete API, service/store/assistant/MCP legacy fences, atomic relay projection, and all §6b handoffkeep wrk subcommands. | Accepted R4, hk 1398 prerequisite; M1 fixtures. Pair-buildable from identical fixed server brief, isolated test storage only. |
| C1 wrk adapter/projections, hk 1926 | S+ / T3 / builder | Canonical CLI argv transport only; PR242 guard at every operation, persistent 0600 intents/exact retry, claim/assignment spawn order, receipts/projections, typed rounds/void/join/done, no activated fallback, import tooling. | Accepted R4; may build with S1 in parallel against exact CLI/API contract; integrate after S1. Reconcile eventual PR242 merged head. |
| C2 consumers, hk 1927 | S+ / T3 / builder | Every code entry in §6a, across agent-skills/panewire/handoffkeep, must consume canonical receipts; release-time recheck, independent checkpoints, replay/idle/delivery distinction, no protected legacy route or local-event completion/release. S1 implements shared server fence; C2 proves full end-to-end coverage. | S1/C1 contract; joint integration with C1 and M1. §6a is the minimum mandatory inventory and is refreshed at implementation heads. No partial enforcement rollout. |
| A1 attribution, hk 1928 | B / T1 / builder for docs/wrappers; desk for credentials | Dedicated at-integrator/pb-integrator/tt-integrator wrappers and explicit policy entries, four reads only, no claim/grant scope. Desk issues three credentials when task is ready; no values in artifacts. | Accepted R4; required before those wrappers' P4 attribution verification. No OS-user isolation claim. |
| K1 skill/PLAYBOOK gates, hk 1929 | B / T1 / docs builder | Implement §6a documented director/builder and private PLAYBOOK steps for canonical assignment/round/verdict/joined/cleanup reads and independent receipt retention; include typed-grant request route and P2 durability/rollout gates. | Accepted R4; may proceed with A1. Must land before P2; desk owns live private PLAYBOOK operations. |
| D1 server/schema/policy deploy, hk 1920 | Desk-owned operations; S+ / T3 brief; one installer | Additive migration and policy, CLI/server binaries, runtime append-only grants, keep P1 initially. Q12 pre-P2 evidence: durable commits, backup/WAL restore drill conducted by desk, generation/recovery freeze and director-held receipts verified together. Missing evidence blocks P2. | S1 cross-family PASS/required checks, M1; operations only, no builder task or pair deployment. |
| D2 host rollout/import, hk 1921 | Desk-owned operations; S / T3 brief; one owner | Deploy C1/C2/K1 and reconcile PR242; desktop canary then §7 hosts; activate between rounds, batch per-job extension/head-repeat grants after activation, record per-host receipts/24h witness, Q1 hand-off before P2. Protected task closures use desk's authenticated canonical receipt path. | D1 and C1/C2 integration PASS, K1, Q12 evidence; A1 before corresponding P4 checks. One desk execution, no duplicate migrations/builders. |

Delivery order: T0 and R4, then M1, then S1/C1 in parallel, then C2 integration, then D1/D2 desk operations. A1/K1 may start any time after R4 acceptance. S1/C1 builders need no contract negotiation: schemas, state rules, CLI/error/intent formats and vectors are fixed here. Pair builds compare independent implementations; they never authorize concurrent production writes. No install, deploy, merge or PR is performed by R4.


## 10. Decided operator answers

The source record for every row is hk 1922, dr-1922-1 revision 1, answered A, task event 5657 at 2026-10-24T07:25:47.771697849Z; desk comment 7044 and director comment 7046 explicitly enumerate all thirteen answers. No unanswered default remains normative. This approval fixes the design; it authorizes no deployment from this task.

| Answer | Decided behaviour | Record / sections |
|---|---|---|
| Q1=a | Only director-1/operator-desk issue all grant types; pa-integrator asks centrally even for PA. | dr-1922-1=A; §3/§5/§7 |
| Q2=a | Enable typed one-use head_repeat by policy; exact reviewer/round/head, ordinary cap and extension at cap, preserve every verdict. | dr-1922-1=A; §2/§4/§5/§8 |
| Q3=a | Same-host review bytes are host-attested; no individual authorship isolation is claimed. | dr-1922-1=A; §1/§4/§8 |
| Q4=a | Dedicated at-integrator/pb-integrator/tt-integrator ids for attribution only; no claim/grant scope; desk issues three credentials when hk 1928 is ready. | dr-1922-1=A and desk 7044; §1/§3/§5/§7/§9 |
| Q5=a | No offline waiver; preserve report/intents locally, pause authority steps until server recovery. | dr-1922-1=A; §6/§6b |
| Q6=a | Archive local PASS/grants as untrusted, imported baseline at least 3; fresh server review plus extension, activated between rounds with batched grants. | dr-1922-1=A; §2/§5/§6/§7 |
| Q7=a | Root/builder/worker claims stay central and precede work; host validates them. Q13 is a scoped review-role exception only. | dr-1922-1=A; §2/§3/§5/§6 |
| Q8=a | Verdict-free void releases only its head reservation; actual verdict path/content/head identity survives; new open counts again. | dr-1922-1=A; §2/§4/§5 |
| Q9=a | T2/T3 joining PASS requires canonical review assignment, checked at open/verdict acceptance and retained after reviewer end; Q13 fixes who mints same-host assignments. | dr-1922-1=A; §2/§4/§5/§6 |
| Q10=a | Original eight ids read fleet evidence; Q4 adds three attribution-only readers. Decoded evidence passes internal/guard before storage; rejection invalid_request never echoes content. | dr-1922-1=A; §3/§5/§6a |
| Q11=a | No pre-O3 rollback while active jobs exist; freeze/recover with fences intact. | dr-1922-1=A; §6/§7 |
| Q12=a | Durable commits, desk-tested backup/restore, generation fence and independent director receipt retention verified before P2; otherwise remain shadow/O2. | dr-1922-1=A and desk 7044; §7/§8/§9 |
| Q13=a | Host mints only fresh tester/reviewer/counsel assignment for its own active subject, same task, pinned fields and builder parent lane; no grant/cap effect. Central cross-host placement remains available. | dr-1922-1=A; §2/§3/§4/§5/§6 |

Revision note: r1 was published with sha256 9459ffefe80cd9da06cbeac6aed1d6910ac84122a94f68c61ff77710baf12257. Independent Opus review round 1 returned BLOCKER with B1 (undefined policy grammar) and B2 (shadow activation/seal semantics). R2 adds the Policy schema/hash/permission/lane contract and an explicit import freeze/seal state machine plus vectors. It also resolves S1–S8 and N1–N5 by clarifying voided-head reuse, reviewer binding, request hashing, verdict grammar, client-side regression detection, legacy route coverage, payload consistency and canonically assigned T2/T3 PASS requirements. Reviewer files were not modified. The then-current at/pb/tt addition was explicit in §1; Q4 now supersedes its deferred status. R2 review resolved B1/B2, found R2-B1 (reviewer activeness ambiguous at join), and reported residual S1–S3/N1–N3. R3 fixes activeness at open/verdict acceptance only and adds PASS -> tester end -> join acceptance; it explicitly expires historical-open head reservations, serializes lane mutations under one global advisory lock, limits whitespace/CR header restrictions to the header, encodes per-client permission ceilings in Policy, pins claim -> activate -> pane ordering, and names the operator-desk closure route. Under the director's design-only-round ruling, only round 1 verdict is wrk-recorded; later rounds are recorded on hk 1915 and in the delivery report. For r1–r3 the fixed review code head was AS; R4 retains that review anchor and refreshes HK/PW and the explicit pending-AS242 citations.


Revision 4 note (hk 1923; each required change is traced here):

1. All thirteen answers from hk 1922 dr-1922-1=A replace undecided recommendations/defaults (§1–§8, §10 table). Q2 requires head_repeat_enabled=true; Q4 creates three attribution-only ids; Q12 requires desk's completed restore drill and all durability witnesses before P2. Q1 removes pa-integrator grant authority and specifies the O3-PA-GRANT-REQUEST hand-off (§3/§7).
2. G1/Q13 adds scoped host reviewer.claim.write, the derived claim payload/lane/role and live-assignment rules, no separate review budget or central activation hop, exact error outcomes, matrix row and spawn ordering (§2/§3/§4/§5/§6). Root/builder/worker/correction/successor minting remains central under Q7. New vectors test foreign host, ended subject, different task, field/role/grant/parent/id/slot escalation and acceptance-time assignment checks.
3. G2 enumerates the receipt consumers with immutable repository/file/function citations, C2 ownership and K1 process gates, including delegated Go paths, sentinel/reap/prune/pass-4/takeover releases, delivery replay/idle-wake/counting, handoffkeep service/store/assistant/MCP and director/builder/PLAYBOOK procedures (§6a; §9 C2/K1).
4. G3 fixes the transport as new S1 handoffkeep wrk subcommands only, with complete arguments/stdin/stdout JSON bindings, safe error/exit map and canonical binary checks on every call; C1 owns the durable 0600 job-dir intent and exact key/body reuse (§6b/§9). No token reading or HTTP implementation/fallback is added to wrk. Transport and crash/error vectors are normative.
5. The delivery table now names T0 1922, R4 1923, M1 1924, S1 1925, C1 1926, C2 1927, A1 1928, K1 1929, D1 1920 and D2 1921; preserves S1/C1/C2 S+, M1 A, A1 B/T1 and K1 B/T1/docs; D1/D2 are desk-owned ops (§9).
6. S-a refreshes every current-code citation to AS/HK/PW or explicitly pending AS242 and states PR242's canonical trust-path changes (§header/§1/§6a/§6b/§7). S-b schedules activation between rounds and batches per-job extension/repeat grants after activation (§6/§7/§9). S-c names internal/guard.Reject and tests generated token-shaped verdict rejection without storage/echo (§5). S-d gives pa-integrator the central hand-off (§3/§7).

Preserved: record kinds/model, hash chain, digest normalization, CAS, successful-key replay, idempotency namespace, existing server error map and import freeze/seal/activate accounting. Q13 adds claim-role semantics/permission and a review-role lane-routing and lifecycle-protection exception, not a new state machine or budget. Canonical reviewer completed/end are receipt-required from claim even while summary.state=shadow; subject import/activate accounting is unchanged. Q4 necessarily extends Policy's closed client list/matrix by three read-only ids. Q2 necessarily changes Policy head_repeat_enabled from boolean to const true and updates the single R3 disabled-feature regression vector to test missing grant plus invalid disabled policy; every other R3 regression vector is preserved. General mutation prose now explicitly places mutable target validation after successful-key replay/CAS, as the preserved replay rule requires for Q13 mint retries. No evidence file or previous published revision was edited.

Validation: Python jsonschema 4.10.3, Draft202012Validator.check_schema plus local-ref resolution and positive/negative Input_claim/Policy fixtures; exact command WRK_TEST_MODE=1 python3 /home/mgh3326/work/herdr-inbox/jobs/b1923-sol/validate_design.py /home/mgh3326/work/herdr-inbox/jobs/b1923-sol/design.md /tmp/b1923-validation. This checks the document contract, not an implementation of the proposed server. Independent review rounds and final content hash are recorded in the delivery report, not claimed by this validation.


R4 review amendment 1 (after hk 1923 review round 1, B1; document remains revision 4): B1 is closed by the monotonic role-derived receipt_required rule in §2, applied to §4/§5 relay fences, §6 C1 fail-closed paths/P1 spawn, §6a all completion/quota consumers and §7 rollback. No reviewer activate or own budget is added, and no record/schema/state field changes. Added vectors cover reviewer 5xx, missing legacy receipts, own canonical completion/end cleanup, liveness, P1 and subject-task receipt binding. Related SHOULDs S1–S5 are addressed: missing spawn compensation/quota-report/process consumers and K1 enumeration (§6a), explicit central P1 assignment staging (§6), C1 credential/endpoint environment scrub (§6b), protocol-invalid/5xx pending-intent handling (§6b), and task receipts bound to the current subject rather than a review job sharing task_id (§4/§5/§6a). S6 remains an explicit availability limit (§8), without a new naming policy. N2/N3/N4/N5 clarifications name the redirect override, exact delegated call, conservative guard-rejection manifest stop and mint-intent job directory. N1's endpoint Error linkage is a formatting follow-up only: the complete error map remains normative and unchanged. Every required change 1–6 in the revision-4 note above still applies. Reviewer's files were not edited.
