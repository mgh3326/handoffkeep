package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	_ "github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgtype"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/mgh3326/handoffkeep/internal/mysqlurl"
)

// CopyTable is the copy contract for one table: column order is read from the
// schema's ordinal positions and then fixed for both hashing and insertion.
type CopyTable struct {
	Name       string `json:"name"`
	SourceRows int64  `json:"source_rows"`
	TargetRows int64  `json:"target_rows"`
	SourceHash string `json:"source_sha256"`
	TargetHash string `json:"target_sha256"`
	Match      bool   `json:"match"`
}

// CopyReport reports copied and intentionally skipped schema tables.
type CopyReport struct {
	From    string      `json:"from"`
	To      string      `json:"to"`
	Tables  []CopyTable `json:"tables"`
	Skipped []string    `json:"skipped"`
	Match   bool        `json:"match"`
}

type copyTableSpec struct {
	name    string
	columns []string
	order   []string
	bounds  map[string]int
	// pgIdentity marks a table whose id is GENERATED ALWAYS AS IDENTITY on
	// PostgreSQL (yt_write_ops): a PostgreSQL target must take the explicit id
	// with OVERRIDING SYSTEM VALUE.
	pgIdentity bool
	// serial marks a table with a generated key (BIGSERIAL, AUTO_INCREMENT):
	// after the load the target's generator is moved past the largest id.
	serial bool
	// identity is the generated column name when it differs from id.
	identity string
	// parentColumn names a self-referencing foreign key. Rows are inserted
	// parents first (by depth, then order), because the foreign key is enforced
	// on both targets while the table loads. Rows no root reaches (a cycle)
	// are not selected, and the row-count parity then fails the table.
	parentColumn string
	// deferredColumns names nullable self-references filled in after every row
	// has been inserted. This supports tables with multiple independent self-FKs.
	deferredColumns []string
	// upsert marks a table that a fresh schema already seeds (task_projects):
	// the load overwrites a row with the same key instead of failing on it.
	upsert bool
}

type copyQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

type copyValueKind uint8

const (
	copyText copyValueKind = iota
	copyInteger
	copyFloat
	copyBoolean
	copyTime
	copyJSON
	copyTextArray
)

// Ported copy registry. Keep quota_observations before quota_latest because
// quota_latest references it; areas add their tables here when they port.
var copyTableRegistry = []copyTableSpec{
	{name: "attachments", columns: strings.Fields("sha256 size_bytes mime original_name created_by created_at object_location archived_at archive_location hot_deleted_at"), order: []string{"sha256"}, bounds: map[string]int{"sha256": 64}},
	{name: "attachment_refs", columns: strings.Fields("sha256 ref_kind ref_id"), order: []string{"sha256", "ref_kind", "ref_id"}, bounds: map[string]int{"sha256": 64, "ref_kind": 16, "ref_id": 512}},
	{name: "attachment_uploads", columns: strings.Fields("id staging_key name mime size_bytes sha256 ref_kind ref_id created_by status reason created_at expires_at updated_at staging_deleted_at claim_nonce"), order: []string{"id"}, bounds: map[string]int{"id": 32, "sha256": 64, "ref_kind": 16}},
	{name: "attachment_usage", columns: strings.Fields("month puts gets bytes_added"), order: []string{"month"}, bounds: map[string]int{"month": 7}},
	{name: "bench_catalog", columns: strings.Fields("profile effort model_id pool grade score gate gate_reason benchmark_source benchmark_annotation boundary_version deviation_ref decided_at decided_by retired_at"), order: []string{"profile", "effort"}, bounds: map[string]int{"profile": 200, "effort": 200, "grade": 16, "gate": 32}},
	{name: "bench_grades", columns: strings.Fields("profile grade boundary_version deviation_ref decided_at decided_by"), order: []string{"profile"}, bounds: map[string]int{"profile": 200, "grade": 16}},
	{name: "bench_reps", serial: true, columns: strings.Fields("id origin_id profile model_id task_ref tier role rounds blockers_found completed input_tokens output_tokens notes recorded_at effort grade table_grade created_by created_at experiment_id arm first_ci_green critical_findings high_findings merged reverted_within_7d reviewer_profile reviewer_effort reasoning_tokens cache_read_tokens cache_write_tokens spawn_to_ready_s spawn_to_merge_s"), order: []string{"id"}, bounds: map[string]int{"created_by": 128}},
	{name: "quota_observations", serial: true, columns: strings.Fields("id account_key kind window_key measured_at value received_at"), order: []string{"id"}, bounds: map[string]int{"account_key": 64, "kind": 16, "window_key": 16}},
	{name: "quota_latest", columns: strings.Fields("account_key kind window_key observation_id measured_at confirmed_at"), order: []string{"account_key", "kind", "window_key"}, bounds: map[string]int{"account_key": 64, "kind": 16, "window_key": 16}},
	{name: "quota_leases", columns: strings.Fields("account_key owner holder expires_at"), order: []string{"account_key"}, bounds: map[string]int{"account_key": 64}},
	{name: "quota_presence", columns: strings.Fields("host_id account_key last_seen"), order: []string{"host_id", "account_key"}, bounds: map[string]int{"host_id": 64, "account_key": 64}},
	{name: "jobs", columns: strings.Fields("job_id task_id host state role label family profile model pool owner_lane parent_lane parent_job pane_id worktree report_doc result created_by created_at updated_at input_tokens output_tokens reasoning_tokens cache_read_tokens cache_write_tokens usage_source usage_recorded_at harness effort claimed_at spawned_at spawned_received_at ready_at ready_received_at ended_at ended_received_at report_sha256 report_version skills skills_supported end_reason diff_files diff_insertions diff_deletions selection owner_node owner_epoch"), order: []string{"job_id"}, bounds: map[string]int{"job_id": 128, "host": 32, "report_sha256": 64}},
	{name: "ledger_cursors", columns: strings.Fields("consumer acked_sync_id acked_at updated_by"), order: []string{"consumer"}, bounds: map[string]int{"consumer": 128}},
	{name: "mcp_write_audit", serial: true, columns: strings.Fields("id tool actor channel author task_id idempotency_key result at"), order: []string{"id"}, bounds: map[string]int{"tool": 64, "actor": 320, "channel": 32, "author": 128, "idempotency_key": 128}},
	{name: "notification_outbox", serial: true, columns: strings.Fields("id kind target_lane event_id text created_at sent_at hub_row_id applied_at applied_by skipped_at skip_reason"), order: []string{"id"}, bounds: map[string]int{"kind": 32, "target_lane": 255, "event_id": 256, "applied_by": 128, "skip_reason": 128}},
	{name: "placement_decisions", serial: true, columns: strings.Fields("id job_id attempt task_id decided_at decided_on tool_version source ranking state_source chosen_host outcome reason profile pool account_key family model role pane_id repo candidates"), order: []string{"id"}, bounds: map[string]int{"job_id": 128, "decided_on": 128}},
	// The tasks area (hk 5074). Parents before children: foreign keys stay
	// enforced on both targets. tasks refers to itself and to project_milestones.
	{name: "task_projects", upsert: true, columns: strings.Fields("name created_by created_at status start_at target_at design_lane"), order: []string{"name"}, bounds: map[string]int{"name": 128}},
	{name: "project_milestones", serial: true, columns: strings.Fields("id project name description target_at status sort_order"), order: []string{"id"}, bounds: map[string]int{"project": 128}},
	{name: "project_owner_events", serial: true, columns: strings.Fields("id project previous_lane design_lane actor note created_at"), order: []string{"id"}, bounds: map[string]int{"project": 128}},
	{name: "relay_events", serial: true, columns: strings.Fields("id kind job_id epoch owner_lane machine pane_id report_path report_last_line question pr head reason event_id text event_time received_at delivered_at delivered_to attempts"), order: []string{"id"}, bounds: map[string]int{"kind": 32, "job_id": 128, "owner_lane": 255, "event_id": 256}},
	{name: "tasks", serial: true, parentColumn: "parent_id", columns: strings.Fields("id lane parent_lane title kind state priority refs claimed_by created_by created_at updated_at body_doc project area expected source source_ref last_activity_at awaiting_decision parent_id started_at completed_at canceled_at triaged_at started_triage_at milestone_id due_at sla_started_at sla_medium_risk_at sla_high_risk_at sla_breaches_at close_rule"), order: []string{"id"}, bounds: map[string]int{"lane": 255, "parent_lane": 255, "kind": 16, "state": 16, "project": 128, "source": 128, "source_ref": 512, "close_rule": 16}},
	{name: "documents", serial: true, columns: strings.Fields("id key kind session job body sha256 created_by created_at updated_at task_id origin_class"), order: []string{"id"}, bounds: map[string]int{"key": 512, "kind": 32, "session": 128, "job": 128, "sha256": 64, "origin_class": 16}},
	{name: "task_comments", serial: true, columns: strings.Fields("id task_id body author created_at source_ref system_event_key"), order: []string{"id"}, bounds: map[string]int{"source_ref": 128, "system_event_key": 255}},
	{name: "translations", columns: strings.Fields("entity entity_id field lang source_sha256 text translator updated_at"), order: []string{"entity", "entity_id", "field", "lang"}, bounds: map[string]int{"entity": 32, "entity_id": 128, "field": 32, "lang": 8, "source_sha256": 64}},
	{name: "task_events", serial: true, columns: strings.Fields("id task_id from to by note at refs kind no_job source source_ref field from_value to_value auto applied"), order: []string{"id"}, bounds: map[string]int{"kind": 32, "source": 128, "source_ref": 512}},
	{name: "task_relations", serial: true, columns: strings.Fields("id kind src_id dst_id created_by created_at archived_at archived_by"), order: []string{"id"}, bounds: map[string]int{"kind": 16}},
	{name: "task_origins", serial: true, columns: strings.Fields("id task_id provider namespace external_id display_ref url title_snapshot snapshot_at created_by created_at tenant role synced_at"), order: []string{"id"}, bounds: map[string]int{"provider": 16, "namespace": 255, "external_id": 255, "role": 16}},
	{name: "task_orders", columns: strings.Fields("id task_id words channel actor verified idempotency_key created_at target_lane acked_at acked_by ack_comment_id withdrawn_at withdrawn_by withdraw_reason"), order: []string{"id"}, bounds: map[string]int{"id": 32, "channel": 16, "actor": 320, "idempotency_key": 128, "target_lane": 255, "acked_by": 320, "withdrawn_by": 320}},
	{name: "task_doc_refs", columns: strings.Fields("task_id doc_key created_at created_by"), order: []string{"task_id", "doc_key"}, bounds: map[string]int{"doc_key": 512}},
	{name: "ticket_sets", serial: true, columns: strings.Fields("id source source_ref payload_sha256 parent_task_id created_at"), order: []string{"id"}, bounds: map[string]int{"source": 128, "source_ref": 512, "payload_sha256": 64}},
	{name: "task_spawn_records", columns: strings.Fields("task_id request_id job role state reason attempt host next_check_at expires_at updated_at start_task_id start_until"), order: []string{"task_id"}, bounds: map[string]int{"state": 16, "start_until": 16}},
	{name: "task_spawn_notices", columns: strings.Fields("notice_key task_id created_at"), order: []string{"notice_key"}, bounds: map[string]int{"notice_key": 255}},
	{name: "task_waits", serial: true, columns: strings.Fields("id task_id on_kind on_ref reason since source cleared_at cleared_by clear_note"), order: []string{"id"}, bounds: map[string]int{"on_kind": 16, "on_ref": 256, "source": 160, "cleared_by": 128}},
	{name: "lane_looks", serial: true, columns: strings.Fields("id lane notice_event_id findings_hash note actor created_at"), order: []string{"id"}, bounds: map[string]int{"lane": 255, "notice_event_id": 256, "findings_hash": 16, "actor": 320}},
	{name: "task_thread_deliveries", columns: strings.Fields("task_id destination thread_id thread_name start_event_id since last_event_id posted state claim_key claimed_at next_attempt_at attempts last_error created_at updated_at"), order: []string{"task_id", "destination"}, bounds: map[string]int{"destination": 16, "thread_id": 32, "state": 16}},
	{name: "task_thread_cursors", columns: strings.Fields("destination epoch_event_id scan_event_id updated_at"), order: []string{"destination"}, bounds: map[string]int{"destination": 16}},
	{name: "sync_decisions", columns: strings.Fields("id task_id updated_at"), order: []string{"id"}, bounds: map[string]int{"id": 512}},
	// The changes/deploy area (hk 5078). Parent rows precede their dependents;
	// all generated ids are preserved and reset past the copied maximum.
	{name: "changes", serial: true, deferredColumns: []string{"reverted_by_change_id", "depends_on_change_id", "depends_on_state"}, columns: strings.Fields("id repo pr_number state head_sha base_sha merge_sha opened_at merged_at reverted_by_change_id priority hold_reason depends_on_change_id depends_on_state created_at updated_at draft base_ref title author state_observed_at state_source state_delivery_id checks_observed_at checks_source threads_observed_at threads_source findings_observed_at findings_source gate_state gate_head gate_reasons gate_decided_at gate_policy_revision gate_source"), order: []string{"id"}, bounds: map[string]int{"repo": 766, "state": 16, "head_sha": 64, "base_sha": 64, "merge_sha": 64, "state_delivery_id": 255, "gate_head": 64}},
	{name: "change_tasks", columns: strings.Fields("change_id task_id relation created_at"), order: []string{"change_id", "task_id"}, bounds: map[string]int{"relation": 16}},
	{name: "reviews", serial: true, columns: strings.Fields("id change_id reviewed_head reviewed_base patch_id round pass lens reviewer_job_id family profile model harness effort executor fallback policy_revision contract_source verdict critical high medium low started_at finished_at recorded_at reviewer same_family delta quiet outside_diff_blocking patch_digest"), order: []string{"id"}, bounds: map[string]int{"reviewed_head": 64, "reviewed_base": 64, "lens": 64, "reviewer_job_id": 255, "family": 64, "effort": 32, "verdict": 32, "patch_digest": 128}},
	{name: "change_checks", columns: strings.Fields("change_id head_sha name state conclusion url required started_at completed_at observed_at source gone delivery_id"), order: []string{"change_id", "head_sha", "name"}, bounds: map[string]int{"head_sha": 64, "name": 512, "source": 16, "delivery_id": 255}},
	{name: "change_threads", columns: strings.Fields("change_id thread_id url path line author author_kind severity resolved outdated replied comment_count last_comment_at excerpt redacted observed_at source gone"), order: []string{"change_id", "thread_id"}, bounds: map[string]int{"thread_id": 700, "author_kind": 16, "severity": 16, "source": 16}},
	{name: "change_findings", columns: strings.Fields("change_id reviewed_head round pass lens ordinal review_id ext_id location severity blocking summary url author_replied thread_id body redacted observed_at source"), order: []string{"change_id", "reviewed_head", "round", "pass", "lens", "ordinal"}, bounds: map[string]int{"reviewed_head": 64, "lens": 64, "ext_id": 512, "severity": 16, "thread_id": 512, "source": 16}},
	{name: "verifications", serial: true, columns: strings.Fields("id change_id head_sha level verifier verifier_job_id family profile model harness evidence ci_result source recorded_by recorded_at"), order: []string{"id"}, bounds: map[string]int{"head_sha": 64, "level": 32, "verifier": 128, "verifier_job_id": 128, "family": 128, "profile": 128, "model": 128, "harness": 128, "ci_result": 16, "source": 16, "recorded_by": 128}},
	{name: "change_gate_events", serial: true, columns: strings.Fields("id change_id head_sha state changed reasons policy_revision decided_at source recorded_at"), order: []string{"id"}, bounds: map[string]int{"head_sha": 64, "state": 16, "source": 16}},
	{name: "deploy_record_projection", columns: strings.Fields("key sha256 record decoded"), order: []string{"key"}, bounds: map[string]int{"key": 767, "sha256": 64}},
	{name: "deploy_services", upsert: true, columns: strings.Fields("name repo record_prefixes active created_at updated_at"), order: []string{"name"}, bounds: map[string]int{"name": 128}},
	{name: "dots_results", serial: true, parentColumn: "superseded_by", columns: strings.Fields("id document_key comment_id issue_uuid issue_identifier kind verdict artefact_sha256 task_id routed_lane adoption_state adopted_ref adopted_by adopted_at superseded_by answered_at answered_by answer_comment_id answer_text ask_operator canva_url canva_page canva_version created_at updated_at"), order: []string{"id"}, bounds: map[string]int{"document_key": 512, "comment_id": 255, "issue_uuid": 255, "issue_identifier": 255, "kind": 16, "artefact_sha256": 64, "routed_lane": 255, "adoption_state": 32}},
	{name: "dots_run_tokens", serial: true, columns: strings.Fields("id task_id token issue_key takeover issued_by issued_at superseded_at"), order: []string{"id"}, bounds: map[string]int{"token": 64, "issue_key": 64}},
	{name: "decision_requests", serial: true, columns: strings.Fields("id source source_ref task_id job_id lane question reading grounds options recommended default_action due_at snoozed_until expires_at status answer answered_by answered_at superseded_by created_at updated_at"), order: []string{"id"}, bounds: map[string]int{"source": 32, "source_ref": 512, "job_id": 255, "lane": 255, "status": 16}},
	{name: "decision_answer_attempts", serial: true, columns: strings.Fields("id channel event_key payload_hash task_id request_id revision_request_id question_revision provenance actor result event_at received_at option_id text operator_quote denies_attempt_id receipt"), order: []string{"id"}, bounds: map[string]int{"channel": 16, "event_key": 512, "payload_hash": 64, "request_id": 512, "revision_request_id": 512, "provenance": 32, "result": 32}},
	{name: "decision_resolutions", serial: true, identity: "resolution_id", columns: strings.Fields("resolution_id request_id question_revision revision_request_id task_id kind option_id text receipt provenance channel decided_by recorded_by attempt_id committed_at"), order: []string{"resolution_id"}, bounds: map[string]int{"request_id": 512, "revision_request_id": 512, "kind": 24, "provenance": 32}},
	{name: "decision_deliveries", columns: strings.Fields("request_id destination message_id desired_version applied_version state attempts next_attempt_at last_error_code posted_at edited_at created_at"), order: []string{"request_id", "destination"}, bounds: map[string]int{"destination": 16, "message_id": 255, "desired_version": 16, "applied_version": 16, "state": 16}},
	{name: "decision_notify_digest", columns: strings.Fields("day state listed message_id attempts next_attempt_at last_error sent_at created_at"), order: []string{"day"}, bounds: map[string]int{"state": 16, "message_id": 255}},
	{name: "chat_questions", columns: strings.Fields("id conversation_id lane body state created_at updated_at resolved_at revision answer_message_id"), order: []string{"id"}, bounds: map[string]int{"id": 64, "conversation_id": 255, "lane": 255, "state": 16}},
	{name: "chat_messages", serial: true, columns: strings.Fields("id conversation_id author body source_channel origin_event_id origin_timestamp semantic_hash relay_state created_at delivered_at"), order: []string{"id"}, bounds: map[string]int{"conversation_id": 255, "author": 16, "source_channel": 64, "origin_event_id": 255, "semantic_hash": 64, "relay_state": 16}},
	{name: "chat_message_questions", columns: strings.Fields("message_id question_id relation_kind question_text"), order: []string{"message_id", "question_id", "relation_kind"}, bounds: map[string]int{"question_id": 64, "relation_kind": 16}},
	{name: "operator_inbox_state", columns: strings.Fields("item_key read_at done_at snoozed_until actor updated_at"), order: []string{"item_key"}, bounds: map[string]int{"item_key": 192, "actor": 320}},
	{name: "operator_inbox_events", columns: strings.Fields("event_key item_key action previous_read_at previous_done_at previous_snoozed_until previous_request_id previous_request_snoozed_until applied_read_at applied_done_at applied_snoozed_until undone_at actor at"), order: []string{"event_key", "item_key"}, bounds: map[string]int{"event_key": 128, "item_key": 192, "action": 16, "actor": 320}},
	{name: "sync_state", upsert: true, columns: strings.Fields("singleton max_sync_id pruned_through"), order: []string{"singleton"}},
	{name: "sync_log", serial: true, identity: "entry_id", columns: strings.Fields("entry_id sync_id model model_id action at"), order: []string{"entry_id"}, bounds: map[string]int{"model": 32, "model_id": 512}},
	{name: "checkpoints", serial: true, columns: strings.Fields("id session kind title body refs created_by created_at"), order: []string{"id"}, bounds: map[string]int{"session": 255, "kind": 16}},
	{name: "memory", columns: strings.Fields("agent name description memory_type content updated_by updated_at"), order: []string{"agent", "name"}, bounds: map[string]int{"agent": 255, "name": 255, "memory_type": 16}},
	{name: "yt_write_ops", serial: true, pgIdentity: true, columns: strings.Fields("id principal kind idem_key payload_sha payload entity_kind entity_id yt_target_id actor parent_id child_seq state yt_result_id reason last_error attempts reconciles created_at dispatched_at next_attempt_at resolved_at tenant"), order: []string{"id"}, bounds: map[string]int{"principal": 200, "kind": 64, "idem_key": 200, "payload_sha": 128, "entity_kind": 64, "entity_id": 200, "yt_target_id": 64, "actor": 200, "state": 16, "yt_result_id": 64, "reason": 200, "last_error": 2000, "tenant": 255}, parentColumn: "parent_id"},
	{name: "yt_write_op_requeues", serial: true, pgIdentity: true, columns: strings.Fields("id op_id requeued_at by_operator prior_attempts prior_reason prior_last_error prior_resolved_at"), order: []string{"id"}, bounds: map[string]int{"by_operator": 200, "prior_reason": 200, "prior_last_error": 2000}},
	{name: "yt_entity_map", columns: strings.Fields("entity_kind hk_key yt_id yt_parent_id op_id created_at tenant"), order: []string{"entity_kind", "hk_key"}, bounds: map[string]int{"entity_kind": 64, "hk_key": 200, "yt_id": 64, "yt_parent_id": 64, "tenant": 255}},
	{name: "yt_task_bindings", columns: strings.Fields("task_id tenant issue_id issue_readable project_id bound_by bound_at comments_from origin op_id"), order: []string{"task_id"}, bounds: map[string]int{"tenant": 255, "issue_id": 64, "issue_readable": 64, "project_id": 64, "bound_by": 200, "origin": 32}},
	{name: "yt_comment_canary", columns: strings.Fields("task_id tenant issue_id issue_readable project_id added_by added_at"), order: []string{"task_id"}, bounds: map[string]int{"tenant": 255, "issue_id": 64, "issue_readable": 64, "project_id": 64, "added_by": 200}},
	{name: "yt_comment_write_links", columns: strings.Fields("comment_id op_id created_at"), order: []string{"comment_id"}},
	{name: "yt_authority", upsert: true, columns: strings.Fields("kind authority switched_at switched_by note"), order: []string{"kind"}, bounds: map[string]int{"kind": 16, "authority": 16, "switched_by": 200, "note": 2000}},
	{name: "yt_write_op_alerts", columns: strings.Fields("op_id alert_class created_at"), order: []string{"op_id", "alert_class"}, bounds: map[string]int{"alert_class": 16}},
	{name: "yt_write_op_adoptions", columns: strings.Fields("op_id attempts yt_id recorded_at"), order: []string{"op_id"}, bounds: map[string]int{"yt_id": 64}},
	{name: "yt_issue_comment_bounds", columns: strings.Fields("op_id task_id comments_from recorded_at"), order: []string{"op_id"}},
	{name: "yt_issue_intents", columns: strings.Fields("task_id tenant recorded_at"), order: []string{"task_id"}, bounds: map[string]int{"tenant": 255}},
	{name: "yt_binding_history", serial: true, columns: strings.Fields("id task_id tenant issue_id origin action actor reason at"), order: []string{"id"}, bounds: map[string]int{"tenant": 255, "issue_id": 64, "origin": 32, "action": 16, "actor": 200, "reason": 2000}},
	{name: "youtrack_comment_links", columns: strings.Fields("hk_comment_id task_id youtrack_comment_id created_at"), order: []string{"hk_comment_id"}, bounds: map[string]int{"youtrack_comment_id": 512}},
	// The rest of the YouTrack area (hk 5077). Parents before children:
	// yt_family_roots references yt_write_ops above. youtrack_projects,
	// yt_binding_cursor and yt_links_sweep are seeded by a fresh schema, so
	// the load upserts them.
	{name: "youtrack_projects", upsert: true, columns: strings.Fields("hk_project yt_project_id short_name created_at updated_at"), order: []string{"hk_project"}, bounds: map[string]int{"hk_project": 128, "yt_project_id": 64, "short_name": 64}},
	{name: "yt_article_bindings", columns: strings.Fields("document_id tenant article_id article_readable project_id part generation content_sha origin op_id needs_update bound_by bound_at"), order: []string{"document_id"}, bounds: map[string]int{"tenant": 255, "article_id": 64, "article_readable": 64, "project_id": 64, "content_sha": 64, "origin": 16, "bound_by": 200}},
	{name: "yt_snapshot_state", columns: strings.Fields("entity_kind entity_id kind seq last_sha last_op_id"), order: []string{"entity_kind", "entity_id", "kind"}, bounds: map[string]int{"entity_kind": 64, "entity_id": 512, "kind": 64}},
	{name: "yt_feed_cursors", columns: strings.Fields("consumer last_sync_id updated_at"), order: []string{"consumer"}, bounds: map[string]int{"consumer": 64}},
	{name: "yt_binding_cursor", upsert: true, columns: strings.Fields("id last_comment_id"), order: []string{"id"}},
	{name: "yt_binding_intents", columns: strings.Fields("task_id tenant state reason attempts last_error next_attempt_at recorded_at"), order: []string{"task_id"}, bounds: map[string]int{"tenant": 255, "state": 16, "reason": 200, "last_error": 2000}},
	{name: "yt_decision_bindings", columns: strings.Fields("decision_id tenant issue_id issue_readable project_id origin op_id bound_by bound_at"), order: []string{"decision_id"}, bounds: map[string]int{"decision_id": 20, "tenant": 255, "issue_id": 64, "issue_readable": 64, "project_id": 64, "origin": 32, "bound_by": 200}},
	{name: "yt_family_roots", columns: strings.Fields("tenant project_id family article_id op_id created_at"), order: []string{"tenant", "project_id", "family"}, bounds: map[string]int{"tenant": 255, "project_id": 64, "family": 64, "article_id": 64}},
	{name: "yt_link_plans", serial: true, columns: strings.Fields("id op_id attempt recover plan created_at"), order: []string{"id"}},
	{name: "yt_link_steps", columns: strings.Fields("op_id attempt recover step marked_at outcome"), order: []string{"op_id", "attempt", "recover", "step"}, bounds: map[string]int{"outcome": 16}},
	{name: "yt_link_owned", serial: true, columns: strings.Fields("id tenant link_type from_issue to_issue writer_task_id target_task_id kind intended_op_id intended_at released_op_id released_at"), order: []string{"id"}, bounds: map[string]int{"tenant": 255, "link_type": 16, "from_issue": 64, "to_issue": 64, "kind": 16}},
	{name: "yt_link_unowned", serial: true, columns: strings.Fields("id tenant issue_id link_type side other_issue first_seen_op_id last_seen_op_id first_seen_at last_seen_at gone_at"), order: []string{"id"}, bounds: map[string]int{"tenant": 255, "issue_id": 64, "link_type": 16, "side": 32, "other_issue": 64}},
	{name: "yt_links_sweep", upsert: true, columns: strings.Fields("singleton generation done_generation after_task_id updated_at"), order: []string{"singleton"}},
}

// StoreCopyTables returns the currently ported tables in dependency order.
func StoreCopyTables() []string {
	out := make([]string, len(copyTableRegistry))
	for i, t := range copyTableRegistry {
		out[i] = t.name
	}
	return out
}

var copyLeftOutTables = strings.Fields(`deploy_changes deploys labels linear_board_owners linear_board_ownership_conflicts linear_board_tasks linear_capacity_state linear_create_admissions linear_decision_issues linear_decision_ops linear_issues linear_outbox relay_events_retired_20260918 schema_migrations schema_version services standing_orders status_updates task_labels task_state_migration task_subscribers youtrack_attempts youtrack_cursor youtrack_decision_mirrors youtrack_doc_articles youtrack_field_cache youtrack_outbox youtrack_processed_activities`)

type copyDB struct {
	db    *sql.DB
	conn  *sql.Conn
	mySQL bool
}

func openCopyDB(ctx context.Context, raw string) (*copyDB, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("malformed database URL")
	}
	var driver, dsn string
	my := u.Scheme == "mysql"
	switch u.Scheme {
	case "mysql":
		cfg, e := mysqlurl.Parse(raw)
		if e != nil {
			return nil, e
		}
		driver, dsn = "mysql", cfg.FormatDSN()
	case "postgres", "postgresql":
		driver, dsn = "pgx", raw
	default:
		return nil, errors.New("database URL must use postgres:// or mysql://")
	}
	db, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(4)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	c := &copyDB{db: db, mySQL: my}
	return c, nil
}

func (d *copyDB) close() {
	if d.conn != nil {
		_ = d.conn.Close()
	}
	_ = d.db.Close()
}
func (d *copyDB) session(ctx context.Context) (*sql.Conn, error) {
	if d.conn != nil {
		return d.conn, nil
	}
	c, err := d.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	d.conn = c
	return c, nil
}

func prepareCopySession(ctx context.Context, dst *copyDB) (func(), error) {
	if !dst.mySQL {
		return func() {}, nil
	}
	conn, err := dst.session(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, `SET @hk_bulk_load = 1`); err != nil {
		return nil, fmt.Errorf("set MySQL copy bulk-load flag: %w", err)
	}
	return func() { _, _ = conn.ExecContext(context.Background(), `SET @hk_bulk_load = NULL`) }, nil
}

// CopyStore copies the currently ported tables in either direction. It does
// not migrate schemas or truncate destination tables. Table writes are atomic.
func CopyStore(ctx context.Context, fromURL, toURL string, selected []string, verifyOnly bool) (CopyReport, error) {
	report := CopyReport{From: schemeOf(fromURL), To: schemeOf(toURL), Match: true, Skipped: append([]string(nil), copyLeftOutTables...)}
	sort.Strings(report.Skipped)
	wanted, err := selectCopyTables(selected)
	if err != nil {
		return report, err
	}
	from, err := openCopyDB(ctx, fromURL)
	if err != nil {
		return report, err
	}
	defer from.close()
	to, err := openCopyDB(ctx, toURL)
	if err != nil {
		return report, err
	}
	defer to.close()
	if from.mySQL == to.mySQL {
		return report, errors.New("store-copy requires one PostgreSQL and one MySQL URL")
	}
	sourceSnapshot, err := beginCopySnapshot(ctx, from.db)
	if err != nil {
		return report, err
	}
	defer sourceSnapshot.Rollback()
	if !verifyOnly {
		cleanup, err := prepareCopySession(ctx, to)
		if err != nil {
			return report, err
		}
		defer cleanup()
	}
	for _, spec := range copyTableRegistry {
		if !wanted[spec.name] {
			continue
		}
		cols, err := copyColumns(ctx, from.db, from.mySQL, spec.name)
		if err != nil {
			return report, fmt.Errorf("%s source schema: %w", spec.name, err)
		}
		targetCols, err := copyColumns(ctx, to.db, to.mySQL, spec.name)
		if err != nil {
			return report, fmt.Errorf("%s target schema: %w", spec.name, err)
		}
		if !copySchemaMatches(spec.name, cols, spec.columns) || !copySchemaMatches(spec.name, targetCols, spec.columns) {
			return report, fmt.Errorf("%s schema columns differ: source=%v target=%v", spec.name, cols, targetCols)
		}
		sourceKinds, err := copyColumnKinds(ctx, from.db, from.mySQL, spec.name, spec.columns)
		if err != nil {
			return report, fmt.Errorf("%s source column types: %w", spec.name, err)
		}
		targetKinds, err := copyColumnKinds(ctx, to.db, to.mySQL, spec.name, spec.columns)
		if err != nil {
			return report, fmt.Errorf("%s target column types: %w", spec.name, err)
		}
		for _, col := range spec.columns {
			if !compatibleCopyKind(spec.name, col, sourceKinds[col], targetKinds[col]) {
				return report, fmt.Errorf("%s.%s incompatible source and target column types", spec.name, col)
			}
		}
		cols = spec.columns
		var srcCount int64
		srcHash, err := checksum(ctx, sourceSnapshot, from.mySQL, spec, cols, sourceKinds, func(row []any) error {
			srcCount++
			for i, col := range cols {
				if max := spec.bounds[col]; max > 0 {
					if s, ok := row[i].(string); ok && utf8.RuneCountInString(s) > max {
						return fmt.Errorf("refusing %s.%s value of %d characters; MySQL limit is %d", spec.name, col, utf8.RuneCountInString(s), max)
					}
				}
			}
			return nil
		})
		if err != nil {
			return report, fmt.Errorf("%s source read: %w", spec.name, err)
		}
		if !verifyOnly {
			if err := copyRows(ctx, sourceSnapshot, to, from.mySQL, spec, cols, sourceKinds); err != nil {
				return report, fmt.Errorf("%s copy: %w", spec.name, err)
			}
			if err := resetIdentity(ctx, to, spec); err != nil {
				return report, fmt.Errorf("%s identity: %w", spec.name, err)
			}
		}
		tgtCount := int64(0)
		tgtKinds, err := copyColumnKinds(ctx, to.db, to.mySQL, spec.name, spec.columns)
		if err != nil {
			return report, fmt.Errorf("%s target column types: %w", spec.name, err)
		}
		tgtHash, err := checksum(ctx, to.db, to.mySQL, spec, cols, tgtKinds, func([]any) error { tgtCount++; return nil })
		if err != nil {
			return report, fmt.Errorf("%s target read: %w", spec.name, err)
		}
		row := CopyTable{Name: spec.name, SourceRows: srcCount, TargetRows: tgtCount, SourceHash: srcHash, TargetHash: tgtHash, Match: srcCount == tgtCount && srcHash == tgtHash}
		report.Tables = append(report.Tables, row)
		report.Match = report.Match && row.Match
	}
	return report, nil
}

func copySchemaMatches(table string, actual, expected []string) bool {
	if table == "sync_log" {
		filtered := make([]string, 0, len(actual))
		for _, col := range actual {
			if col != "txid" && col != "txn" {
				filtered = append(filtered, col)
			}
		}
		actual = filtered
	}
	return sameCopyColumns(actual, expected)
}

func beginCopySnapshot(ctx context.Context, db *sql.DB) (*sql.Tx, error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin repeatable-read source snapshot: %w", err)
	}
	return tx, nil
}

func selectCopyTables(selected []string) (map[string]bool, error) {
	allowed := map[string]bool{}
	for _, name := range StoreCopyTables() {
		allowed[name] = true
	}
	if len(selected) == 0 {
		selected = StoreCopyTables()
	}
	wanted := map[string]bool{}
	for _, name := range selected {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if !allowed[name] {
			return nil, fmt.Errorf("table %q is not in the ported copy registry", name)
		}
		wanted[name] = true
	}
	if len(wanted) == 0 {
		return nil, errors.New("no tables selected")
	}
	// tasks requires sync_decisions in the same selection. Loads bypass the
	// sync_task_decision trigger on both dialects, so a tasks-only copy would
	// leave the decision projection permanently missing on the target.
	// Refuse before any database is opened instead of silently widening the
	// --tables scope: explicit selections stay explicit.
	if wanted["tasks"] && !wanted["sync_decisions"] {
		return nil, fmt.Errorf("table %q requires %q in the same copy selection", "tasks", "sync_decisions")
	}
	return wanted, nil
}

func sameCopyColumns(actual, expected []string) bool {
	if len(actual) != len(expected) {
		return false
	}
	a, e := append([]string(nil), actual...), append([]string(nil), expected...)
	sort.Strings(a)
	sort.Strings(e)
	return strings.Join(a, "\x00") == strings.Join(e, "\x00")
}

func schemeOf(raw string) string {
	u, _ := url.Parse(raw)
	if u == nil {
		return ""
	}
	return u.Scheme
}

func copyColumns(ctx context.Context, db *sql.DB, mysqlDB bool, table string) ([]string, error) {
	q := `SELECT column_name FROM information_schema.columns WHERE table_schema=current_schema() AND table_name=$1 ORDER BY ordinal_position`
	if mysqlDB {
		q = `SELECT column_name FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name=? ORDER BY ordinal_position`
	}
	rows, err := db.QueryContext(ctx, q, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		cols = append(cols, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(cols) == 0 {
		return nil, fmt.Errorf("table does not exist")
	}
	return cols, nil
}

func copyColumnKinds(ctx context.Context, db *sql.DB, mysqlDB bool, table string, copiedColumns []string) (map[string]copyValueKind, error) {
	q := `SELECT column_name,data_type,udt_name FROM information_schema.columns WHERE table_schema=current_schema() AND table_name=$1`
	if mysqlDB {
		q = `SELECT column_name,data_type,column_type FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name=?`
	}
	rows, err := db.QueryContext(ctx, q, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	wanted := make(map[string]bool, len(copiedColumns))
	for _, column := range copiedColumns {
		wanted[column] = true
	}
	kinds := map[string]copyValueKind{}
	for rows.Next() {
		var name, dataType, detail string
		if err := rows.Scan(&name, &dataType, &detail); err != nil {
			return nil, err
		}
		if !wanted[name] {
			continue
		}
		if !mysqlDB && table == "deploy_record_projection" && name == "record" && dataType == "bytea" {
			// The PostgreSQL projection deliberately stores JSON bytes, not jsonb,
			// so escaped NULs survive. Its MySQL JSON column holds the same payload.
			kinds[name] = copyJSON
			continue
		}
		kind, err := classifyCopyColumn(mysqlDB, dataType, detail)
		if err != nil {
			return nil, fmt.Errorf("column %s: %w", name, err)
		}
		kinds[name] = kind
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return kinds, nil
}

func classifyCopyColumn(mysqlDB bool, dataType, detail string) (copyValueKind, error) {
	if mysqlDB {
		switch strings.ToLower(dataType) {
		case "json":
			return copyJSON, nil
		case "datetime", "timestamp", "date":
			return copyTime, nil
		case "tinyint":
			if strings.EqualFold(detail, "tinyint(1)") {
				return copyBoolean, nil
			}
			return copyInteger, nil
		case "smallint", "mediumint", "int", "integer", "bigint":
			return copyInteger, nil
		case "float", "double", "real", "decimal":
			return copyFloat, nil
		case "char", "varchar", "tinytext", "text", "mediumtext", "longtext", "enum", "set":
			return copyText, nil
		default:
			return 0, fmt.Errorf("unsupported MySQL type %q", dataType)
		}
	}
	switch strings.ToLower(dataType) {
	case "boolean":
		return copyBoolean, nil
	case "smallint", "integer", "bigint":
		return copyInteger, nil
	case "real", "double precision", "numeric":
		return copyFloat, nil
	case "timestamp with time zone", "timestamp without time zone", "date", "time without time zone", "time with time zone":
		return copyTime, nil
	case "json", "jsonb":
		return copyJSON, nil
	case "array":
		if detail == "_text" {
			return copyTextArray, nil
		}
		return 0, fmt.Errorf("unsupported PostgreSQL array type %q", detail)
	case "character", "character varying", "text", "USER-DEFINED":
		return copyText, nil
	default:
		return 0, fmt.Errorf("unsupported PostgreSQL type %q", dataType)
	}
}

// copyArrayColumns are PostgreSQL TEXT[] columns that are JSON arrays of
// strings on MySQL. They are compared and converted as a list of strings.
var copyArrayColumns = map[string]bool{
	"deploy_services.record_prefixes": true,
	"task_thread_deliveries.posted":   true,
}

func isCopyArrayColumn(table, column string) bool { return copyArrayColumns[table+"."+column] }

func compatibleCopyKind(table, column string, source, target copyValueKind) bool {
	if source == target {
		return true
	}
	if table == "deploy_record_projection" && column == "record" && source == copyJSON && target == copyJSON {
		return true
	}
	return isCopyArrayColumn(table, column) &&
		(source == copyTextArray && target == copyJSON || source == copyJSON && target == copyTextArray)
}

func quoteCopyIdent(s string, mysqlDB bool) string {
	if mysqlDB {
		return "`" + strings.ReplaceAll(s, "`", "``") + "`"
	}
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func checksum(ctx context.Context, db copyQuerier, mysqlDB bool, spec copyTableSpec, cols []string, kinds map[string]copyValueKind, each func([]any) error) (string, error) {
	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = quoteCopyIdent(c, mysqlDB)
	}
	q := `SELECT ` + strings.Join(quoted, ",") + ` FROM ` + quoteCopyIdent(spec.name, mysqlDB)
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	rowHashes := make([][sha256.Size]byte, 0)
	for rows.Next() {
		vals := make([]any, len(cols))
		dest := make([]any, len(cols))
		for i := range vals {
			dest[i] = &vals[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return "", err
		}
		for i := range vals {
			value, err := canonicalCopyValue(vals[i], spec.name, cols[i], kinds[cols[i]], mysqlDB)
			if err != nil {
				return "", fmt.Errorf("normalize %s.%s: %w", spec.name, cols[i], err)
			}
			vals[i] = value
		}
		b, err := json.Marshal(vals)
		if err != nil {
			return "", err
		}
		rowHashes = append(rowHashes, sha256.Sum256(b))
		if err := each(vals); err != nil {
			return "", err
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return checksumRowHashes(rowHashes), nil
}

func checksumRowHashes(rowHashes [][sha256.Size]byte) string {
	sort.Slice(rowHashes, func(i, j int) bool {
		return bytes.Compare(rowHashes[i][:], rowHashes[j][:]) < 0
	})
	h := sha256.New()
	for _, rowHash := range rowHashes {
		_, _ = h.Write(rowHash[:])
	}
	return hex.EncodeToString(h.Sum(nil))
}

func canonicalCopyValue(v any, table, column string, kind copyValueKind, mysqlSource bool) (any, error) {
	if isCopyArrayColumn(table, column) {
		var refs []string
		var err error
		if mysqlSource {
			refs, err = parseJSONTextArray(v)
		} else {
			refs, err = parsePostgresTextArray(v)
		}
		if err != nil {
			return nil, err
		}
		b, err := json.Marshal(refs)
		return json.RawMessage(b), err
	}
	value, err := normalizeCopyValue(v, kind)
	if err != nil {
		return nil, err
	}
	if kind == copyTime {
		if t, ok := value.(time.Time); ok {
			return t.UTC().Format(time.RFC3339Nano), nil
		}
	}
	return value, nil
}

func copyRows(ctx context.Context, src copyQuerier, dst *copyDB, sourceMySQL bool, spec copyTableSpec, cols []string, kinds map[string]copyValueKind) error {
	conn, err := dst.session(ctx)
	if err != nil {
		return err
	}
	qt := quoteCopyIdent(spec.name, sourceMySQL)
	qcols := make([]string, len(cols))
	for i, c := range cols {
		qcols[i] = quoteCopyIdent(c, sourceMySQL)
	}
	order := make([]string, len(spec.order))
	for i, c := range spec.order {
		order[i] = quoteCopyIdent(c, sourceMySQL)
	}
	query := `SELECT ` + strings.Join(qcols, ",") + ` FROM ` + qt + ` ORDER BY ` + strings.Join(order, ",")
	if spec.parentColumn != "" {
		query = parentsFirstQuery(spec, qcols, order, sourceMySQL)
	}
	rows, err := src.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()
	placeholders := make([]string, len(cols))
	for i := range placeholders {
		placeholders[i] = "?"
		if !dst.mySQL {
			placeholders[i] = fmt.Sprintf("$%d", i+1)
		}
	}
	dqcols := make([]string, len(cols))
	for i, c := range cols {
		dqcols[i] = quoteCopyIdent(c, dst.mySQL)
	}
	insert := `INSERT INTO ` + quoteCopyIdent(spec.name, dst.mySQL) + ` (` + strings.Join(dqcols, ",") + `)`
	if !dst.mySQL && spec.pgIdentity {
		insert += ` OVERRIDING SYSTEM VALUE`
	}
	insert += ` VALUES (` + strings.Join(placeholders, ",") + `)`
	if spec.upsert {
		insert += copyUpsertClause(spec, dqcols, dst.mySQL)
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if !dst.mySQL {
		// The PostgreSQL sync emit triggers skip while this is on (migration
		// 20261118-sync-bulk-load-skip). Transaction-local: it ends with the
		// table's transaction. Foreign keys stay enforced.
		if _, err := tx.ExecContext(ctx, `SELECT set_config('handoffkeep.bulk_load','on',true)`); err != nil {
			return fmt.Errorf("set PostgreSQL copy bulk-load flag: %w", err)
		}
	}
	for rows.Next() {
		values := make([]any, len(cols))
		dests := make([]any, len(cols))
		for i := range values {
			dests[i] = &values[i]
		}
		if err := rows.Scan(dests...); err != nil {
			return err
		}
		for i, value := range values {
			if isCopyDeferredColumn(spec, cols[i]) {
				values[i] = nil
				continue
			}
			values[i], err = copyInsertValue(value, spec.name, cols[i], kinds[cols[i]], sourceMySQL, dst.mySQL)
			if err != nil {
				return fmt.Errorf("convert %s.%s: %w", spec.name, cols[i], err)
			}
		}
		if _, err := tx.ExecContext(ctx, insert, values...); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if len(spec.deferredColumns) > 0 {
		if err := copyDeferredSelfRefs(ctx, src, tx, dst.mySQL, sourceMySQL, spec, kinds); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func isCopyDeferredColumn(spec copyTableSpec, column string) bool {
	for _, deferred := range spec.deferredColumns {
		if deferred == column {
			return true
		}
	}
	return false
}

func copyDeferredSelfRefs(ctx context.Context, src copyQuerier, tx *sql.Tx, targetMySQL, sourceMySQL bool, spec copyTableSpec, kinds map[string]copyValueKind) error {
	columns := append([]string{"id"}, spec.deferredColumns...)
	quoted := make([]string, len(columns))
	for i, column := range columns {
		quoted[i] = quoteCopyIdent(column, sourceMySQL)
	}
	query := `SELECT ` + strings.Join(quoted, ",") + ` FROM ` + quoteCopyIdent(spec.name, sourceMySQL) + ` ORDER BY ` + quoted[0]
	rows, err := src.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()
	sets := make([]string, len(spec.deferredColumns))
	for i, column := range spec.deferredColumns {
		sets[i] = quoteCopyIdent(column, targetMySQL) + "=?"
		if !targetMySQL {
			sets[i] = quoteCopyIdent(column, targetMySQL) + fmt.Sprintf("=$%d", i+1)
		}
	}
	whereID := "?"
	if !targetMySQL {
		whereID = fmt.Sprintf("$%d", len(spec.deferredColumns)+1)
	}
	update := `UPDATE ` + quoteCopyIdent(spec.name, targetMySQL) + ` SET ` + strings.Join(sets, ",") + ` WHERE ` + quoteCopyIdent("id", targetMySQL) + `=` + whereID
	for rows.Next() {
		values := make([]any, len(columns))
		dests := make([]any, len(columns))
		for i := range values {
			dests[i] = &values[i]
		}
		if err := rows.Scan(dests...); err != nil {
			return err
		}
		args := make([]any, len(spec.deferredColumns)+1)
		for i, column := range spec.deferredColumns {
			value, err := copyInsertValue(values[i+1], spec.name, column, kinds[column], sourceMySQL, targetMySQL)
			if err != nil {
				return fmt.Errorf("convert %s.%s: %w", spec.name, column, err)
			}
			args[i] = value
		}
		id, err := copyInsertValue(values[0], spec.name, "id", kinds["id"], sourceMySQL, targetMySQL)
		if err != nil {
			return fmt.Errorf("convert %s.id: %w", spec.name, err)
		}
		args[len(args)-1] = id
		if _, err = tx.ExecContext(ctx, update, args...); err != nil {
			return err
		}
	}
	return rows.Err()
}

func copyInsertValue(v any, table, column string, kind copyValueKind, sourceMySQL, targetMySQL bool) (any, error) {
	if isCopyArrayColumn(table, column) {
		var refs []string
		var err error
		if sourceMySQL {
			refs, err = parseJSONTextArray(v)
		} else {
			refs, err = parsePostgresTextArray(v)
		}
		if err != nil {
			return nil, err
		}
		if targetMySQL {
			b, err := json.Marshal(refs)
			return string(b), err
		}
		return pgtype.FlatArray[string](refs), nil
	}
	value, err := normalizeCopyValue(v, kind)
	if err != nil {
		return nil, err
	}
	if kind == copyJSON && value != nil {
		if table == "deploy_record_projection" && column == "record" && !targetMySQL {
			return []byte(value.(json.RawMessage)), nil
		}
		return string(value.(json.RawMessage)), nil
	}
	return value, nil
}

func normalizeCopyValue(v any, kind copyValueKind) (any, error) {
	if v == nil {
		return nil, nil
	}
	switch kind {
	case copyText:
		if b, ok := v.([]byte); ok {
			return string(b), nil
		}
		return v, nil
	case copyInteger:
		switch x := v.(type) {
		case int:
			return int64(x), nil
		case int16:
			return int64(x), nil
		case int32:
			return int64(x), nil
		case int64:
			return x, nil
		case uint64:
			return int64(x), nil
		case []byte:
			return parseCopyInt(string(x))
		case string:
			return parseCopyInt(x)
		default:
			return nil, fmt.Errorf("cannot normalize %T as integer", v)
		}
	case copyFloat:
		switch x := v.(type) {
		case float32:
			return float64(x), nil
		case float64:
			return x, nil
		case int64:
			return float64(x), nil
		case []byte:
			return parseCopyFloat(string(x))
		case string:
			return parseCopyFloat(x)
		default:
			return nil, fmt.Errorf("cannot normalize %T as float", v)
		}
	case copyBoolean:
		switch x := v.(type) {
		case bool:
			return x, nil
		case int64:
			if x == 0 {
				return false, nil
			}
			if x == 1 {
				return true, nil
			}
		case []byte:
			return parseCopyBool(string(x))
		case string:
			return parseCopyBool(x)
		}
		return nil, fmt.Errorf("cannot normalize %T as boolean", v)
	case copyTime:
		if t, ok := v.(time.Time); ok {
			return t.UTC(), nil
		}
		return nil, fmt.Errorf("cannot normalize %T as time", v)
	case copyJSON:
		var data []byte
		switch x := v.(type) {
		case []byte:
			data = x
		case string:
			data = []byte(x)
		case json.RawMessage:
			data = x
		default:
			return nil, fmt.Errorf("cannot normalize %T as JSON", v)
		}
		var value any
		dec := json.NewDecoder(strings.NewReader(string(data)))
		dec.UseNumber()
		if err := dec.Decode(&value); err != nil {
			return nil, err
		}
		b, err := json.Marshal(value)
		return json.RawMessage(b), err
	case copyTextArray:
		return parsePostgresTextArray(v)
	default:
		return nil, fmt.Errorf("unknown copy value kind %d", kind)
	}
}

func parseCopyInt(s string) (int64, error) {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, err
	}
	return n, nil
}

func parseCopyFloat(s string) (float64, error) {
	n, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, err
	}
	return n, nil
}

func parseCopyBool(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true":
		return true, nil
	case "0", "false":
		return false, nil
	default:
		return false, fmt.Errorf("invalid boolean value %q", s)
	}
}

func parsePostgresTextArray(v any) ([]string, error) {
	switch x := v.(type) {
	case []string:
		if x == nil {
			return []string{}, nil
		}
		return x, nil
	case pgtype.FlatArray[string]:
		refs := []string(x)
		if refs == nil {
			return []string{}, nil
		}
		return refs, nil
	case string:
		return decodePostgresTextArray([]byte(x))
	case []byte:
		return decodePostgresTextArray(x)
	default:
		return nil, fmt.Errorf("cannot decode PostgreSQL text array from %T", v)
	}
}

func decodePostgresTextArray(data []byte) ([]string, error) {
	var values pgtype.FlatArray[string]
	plan := pgtype.NewMap().PlanScan(pgtype.TextArrayOID, pgtype.TextFormatCode, &values)
	if plan == nil {
		return nil, errors.New("PostgreSQL text array decoder unavailable")
	}
	if err := plan.Scan(data, &values); err != nil {
		return nil, err
	}
	if values == nil {
		return []string{}, nil
	}
	return []string(values), nil
}

func parseJSONTextArray(v any) ([]string, error) {
	var data []byte
	switch x := v.(type) {
	case []byte:
		data = x
	case string:
		data = []byte(x)
	default:
		return nil, fmt.Errorf("cannot decode MySQL JSON array from %T", v)
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, errors.New("MySQL JSON text array must be a JSON array")
	}
	var values []string
	if err := json.Unmarshal(data, &values); err != nil {
		return nil, err
	}
	return values, nil
}

// copyUpsertClause makes the insert overwrite a row with the same key. The key
// columns are the table's order columns.
func copyUpsertClause(spec copyTableSpec, quoted []string, mysqlDB bool) string {
	key := map[string]bool{}
	for _, c := range spec.order {
		key[quoteCopyIdent(c, mysqlDB)] = true
	}
	var set []string
	for _, c := range quoted {
		if key[c] {
			continue
		}
		if mysqlDB {
			set = append(set, c+"=VALUES("+c+")")
		} else {
			set = append(set, c+"=EXCLUDED."+c)
		}
	}
	if mysqlDB {
		return " ON DUPLICATE KEY UPDATE " + strings.Join(set, ",")
	}
	keys := make([]string, len(spec.order))
	for i, c := range spec.order {
		keys[i] = quoteCopyIdent(c, false)
	}
	return " ON CONFLICT (" + strings.Join(keys, ",") + ") DO UPDATE SET " + strings.Join(set, ",")
}

// parentsFirstQuery selects a self-referencing table roots first, then each
// level of children, so the target's enforced foreign key never sees a child
// before its parent. WITH RECURSIVE is the same spelling on both dialects.
func parentsFirstQuery(spec copyTableSpec, qcols, order []string, mysqlDB bool) string {
	q := func(c string) string { return quoteCopyIdent(c, mysqlDB) }
	qualified := make([]string, len(qcols))
	for i, c := range qcols {
		qualified[i] = "t." + c
	}
	orderBy := make([]string, len(order))
	for i, c := range order {
		orderBy[i] = "t." + c
	}
	table, parent := q(spec.name), q(spec.parentColumn)
	return `WITH RECURSIVE depth_of(id, depth) AS (SELECT id, 0 FROM ` + table + ` WHERE ` + parent + ` IS NULL UNION ALL SELECT c.id, d.depth + 1 FROM ` + table + ` c JOIN depth_of d ON c.` + parent + ` = d.id) ` +
		`SELECT ` + strings.Join(qualified, ",") + ` FROM ` + table + ` t JOIN depth_of d ON d.id = t.id ORDER BY d.depth, ` + strings.Join(orderBy, ",")
}

func resetIdentity(ctx context.Context, dst *copyDB, spec copyTableSpec) error {
	conn, err := dst.session(ctx)
	if err != nil {
		return err
	}
	table := spec.name
	identity := spec.identity
	if identity == "" {
		identity = "id"
	}
	if !spec.serial {
		return nil
	}
	column := spec.identity
	if column == "" {
		column = "id"
	}
	if dst.mySQL {
		var max sql.NullInt64
		if err := conn.QueryRowContext(ctx, `SELECT MAX(`+quoteCopyIdent(column, true)+`) FROM `+quoteCopyIdent(table, true)).Scan(&max); err != nil {
			return err
		}
		if max.Valid {
			_, err = conn.ExecContext(ctx, `ALTER TABLE `+quoteCopyIdent(table, true)+` AUTO_INCREMENT = `+strconv.FormatInt(max.Int64+1, 10))
		}
		return err
	}
	var seq sql.NullString
	if err := conn.QueryRowContext(ctx, `SELECT pg_get_serial_sequence($1,$2)`, table, column).Scan(&seq); err != nil {
		return err
	}
	if !seq.Valid || seq.String == "" {
		return nil
	}
	quotedIdentity := quoteCopyIdent(column, false)
	if _, err = conn.ExecContext(ctx, `SELECT setval($1, COALESCE((SELECT MAX(`+quotedIdentity+`) FROM `+quoteCopyIdent(table, false)+`), 1), EXISTS(SELECT 1 FROM `+quoteCopyIdent(table, false)+`))`, seq.String); err != nil {
		return err
	}
	if table == "sync_log" {
		// sync_id has its own allocator sequence, separate from the entry_id
		// identity sequence. A fully pruned feed can be empty while sync_state
		// retains a higher watermark, so keep writes past both copied values.
		var last int64
		if err = conn.QueryRowContext(ctx, `SELECT GREATEST(COALESCE((SELECT MAX(sync_id) FROM sync_log),0),COALESCE((SELECT max_sync_id FROM sync_state WHERE singleton),0))`).Scan(&last); err != nil {
			return err
		}
		called := last > 0
		if !called {
			last = 1
		}
		var syncIDSequence string
		if err = conn.QueryRowContext(ctx, `SELECT format('%I.%I',current_schema(),'sync_log_sync_id_seq')`).Scan(&syncIDSequence); err != nil {
			return err
		}
		_, err = conn.ExecContext(ctx, `SELECT setval($1::regclass,$2,$3)`, syncIDSequence, last, called)
	}
	return err
}
