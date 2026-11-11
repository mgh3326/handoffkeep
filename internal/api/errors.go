package api

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mgh3326/handoffkeep/internal/attachments"
	"github.com/mgh3326/handoffkeep/internal/guard"
	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/telemetry"
	"github.com/mgh3326/handoffkeep/internal/wrkrecords"
	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// Every JSON error body is {"error": "<snake_code>"}, optionally with extra
// detail keys ("reason", "pattern", "conflicts", "similar_tasks",
// "request_id"). Handlers
// write a known failure with writeError and hand anything returned by the
// store or a service to appErr, which maps it through errorRules.

func jsonOut(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes the standard error body for a failure the handler has
// already classified.
func writeError(w http.ResponseWriter, status int, code string) {
	jsonOut(w, status, map[string]string{"error": code})
}

// errorRule maps one sentinel error to its HTTP status and wire code. When
// reason is set, the text after "<sentinel text>: " is returned as "reason";
// only set it for sentinels whose reason is store-authored, never echoed
// input.
type errorRule struct {
	err    error
	status int
	code   string
	reason bool
}

// errorRules is the one table from sentinel to response. Order matters only
// where one error could match two rules; the first match wins.
var errorRules = []errorRule{
	{ytwrite.ErrNotFound, http.StatusNotFound, "ytwrite_not_found", false},
	{ytwrite.ErrState, http.StatusConflict, "ytwrite_state", false},
	{ytwrite.ErrInvalid, http.StatusBadRequest, "ytwrite_invalid", false},
	{ytwrite.ErrConflictingResult, http.StatusConflict, "ytwrite_conflicting_result", false},
	{store.ErrStandingNotNewer, http.StatusConflict, "standing_not_newer", false},
	{store.ErrStandingConflict, http.StatusConflict, "standing_conflict", false},
	{store.ErrInvalidSync, http.StatusBadRequest, "invalid_context", false},
	{attachments.ErrDisabled, http.StatusServiceUnavailable, "attachments_disabled", false},
	{attachments.ErrTooLarge, http.StatusRequestEntityTooLarge, "attachment_too_large", false},
	{store.ErrAttachmentStorageCap, http.StatusInsufficientStorage, "attachment_storage_cap", false},
	{store.ErrAttachmentPutCap, http.StatusTooManyRequests, "attachment_put_cap", false},
	{store.ErrSearchTimeout, http.StatusServiceUnavailable, "search_timeout", false},
	{attachments.ErrObjectStore, http.StatusBadGateway, "attachment_object_store_unavailable", false},
	{attachments.ErrDirectUploadDisabled, http.StatusServiceUnavailable, "attachment_direct_upload_disabled", false},
	{store.ErrAttachmentStagingCap, http.StatusInsufficientStorage, "attachment_staging_cap", false},
	{attachments.ErrUploadNotFound, http.StatusNotFound, "attachment_upload_not_found", false},
	{attachments.ErrUploadExpired, http.StatusGone, "attachment_upload_expired", false},
	{attachments.ErrUploadInProgress, http.StatusConflict, "attachment_upload_in_progress", false},
	{attachments.ErrUploadNotReceived, http.StatusConflict, "attachment_upload_not_received", false},
	{attachments.ErrUploadRefused, http.StatusUnprocessableEntity, "attachment_upload_refused", true},
	{store.ErrClosureInvalid, http.StatusBadRequest, "closure_invalid", true},
	{store.ErrTaskConflict, http.StatusConflict, "task_conflict", true},
	{store.ErrTaskMergePath, http.StatusConflict, "task_merge_path", false},
	{store.ErrTaskJobRequired, http.StatusConflict, "task_job_required", true},
	{store.ErrTriageGate, http.StatusConflict, "triage_gate", true},
	{store.ErrTicketSetConflict, http.StatusConflict, "ticket_set_conflict", true},
	{store.ErrTicketSetInvalid, http.StatusBadRequest, "ticket_set_invalid", true},
	{store.ErrDuplicateOfRequired, http.StatusBadRequest, "duplicate_of_required", true},
	{store.ErrTaskParentCycle, http.StatusConflict, "parent_cycle", true},
	{store.ErrTaskParentInvalid, http.StatusBadRequest, "parent_invalid", true},
	{store.ErrRelationCycle, http.StatusConflict, "relation_cycle", true},
	{store.ErrTaskProjectRequired, http.StatusBadRequest, "task_project_required", true},
	{store.ErrTaskProjectUnknown, http.StatusBadRequest, "task_project_unknown", true},
	{store.ErrTaskAreaUnknown, http.StatusBadRequest, "invalid_request", true},
	{store.ErrDispositionOperatorOnly, http.StatusConflict, "disposition_operator_only", false},
	{store.ErrDispositionStale, http.StatusConflict, "disposition_stale", false},
	{store.ErrTaskNotFound, http.StatusNotFound, "not_found", false},
	{store.ErrProjectNotFound, http.StatusNotFound, "project_not_found", false},
	{store.ErrMilestoneNotFound, http.StatusNotFound, "milestone_not_found", false},
	{store.ErrMilestoneProject, http.StatusBadRequest, "milestone_project", false},
	{store.ErrLabelNotFound, http.StatusNotFound, "label_not_found", false},
	{store.ErrLabelParent, http.StatusBadRequest, "label_parent_invalid", false},
	{store.ErrLabelCycle, http.StatusConflict, "label_cycle", false},
	{store.ErrLabelRetired, http.StatusBadRequest, "label_retired", false},
	{store.ErrTaskTerminal, http.StatusConflict, "task_terminal", false},
	{store.ErrDecisionRequestOpen, http.StatusConflict, "decision_request_open", false},
	{store.ErrDecisionRequestStale, http.StatusConflict, "decision_request_stale", false},
	{store.ErrDecisionRequestResolved, http.StatusConflict, "decision_request_resolved", false},
	{store.ErrDecisionEventKeyConflict, http.StatusConflict, "decision_event_key_conflict", false},
	{store.ErrDecisionAttemptNotFound, http.StatusNotFound, "not_found", false},
	{store.ErrDecisionAttemptNotPending, http.StatusConflict, "decision_attempt_not_pending", false},
	{store.ErrDecisionAttemptDenied, http.StatusConflict, "decision_attempt_denied", false},
	{store.ErrRelayEventNotFound, http.StatusNotFound, "not_found", false},
	{store.ErrDeviationRefRequired, http.StatusBadRequest, "deviation_ref_required", false},
	{store.ErrDecidedByRequired, http.StatusBadRequest, "decided_by_required", false},
	{store.ErrBenchCatalogMonotonicity, http.StatusBadRequest, "bench_catalog_not_monotonic", false},
	{store.ErrBenchCatalogScoredSolUnrated, http.StatusBadRequest, "bench_catalog_scored_sol_unrated", false},
	{store.ErrDecisionRefRequired, http.StatusBadRequest, "decision_ref_required", false},
	{store.ErrBenchAreaExceptionProfileUnknown, http.StatusBadRequest, "bench_area_exception_profile_unknown", false},
	{store.ErrBenchAreaExceptionConflict, http.StatusConflict, "bench_area_exception_action_conflict", false},
	{store.ErrBenchRepsRetryable, http.StatusServiceUnavailable, "bench_reps_retryable", false},
	{store.ErrQueueEmpty, http.StatusNotFound, "queue_empty", false},
	{store.ErrChatQuestionNotFound, http.StatusNotFound, "not_found", false},
	{store.ErrChatMessageNotFound, http.StatusNotFound, "not_found", false},
	{store.ErrChatQuestionConflict, http.StatusConflict, "chat_question_conflict", false},
	{store.ErrChatQuestionStale, http.StatusConflict, "chat_question_stale", false},
	{store.ErrDecisionAssistantKind, http.StatusBadRequest, "decision_assistant_kind", false},
	{store.ErrInvalidProvenance, http.StatusBadRequest, "invalid_provenance", false},
	{store.ErrProvenanceRefMismatch, http.StatusConflict, "provenance_ref_mismatch", false},
	{store.ErrDecisionRequestHumanOnly, http.StatusConflict, "decision_request_human_only", true},
	{store.ErrPlacementConflict, http.StatusConflict, "placement_conflict", false},
	{store.ErrJobNotFound, http.StatusNotFound, "not_found", false},
	{store.ErrBenchRepNotFound, http.StatusNotFound, "not_found", false},
	{store.ErrJobIDTaken, http.StatusConflict, "job_id_taken", false},
	{store.ErrJobStateConflict, http.StatusConflict, "job_state_conflict", true},
	{store.ErrOutboxEventNotFound, http.StatusNotFound, "not_found", false},
	{store.ErrOutboxConflict, http.StatusConflict, "notification_outbox_conflict", false},
	{store.ErrChatConversation, http.StatusConflict, "chat_conversation_conflict", false},
	{store.ErrChatMessageConflict, http.StatusConflict, "chat_message_conflict", false},
	{store.ErrDecisionRecordNotFound, http.StatusNotFound, "not_found", false},
	{store.ErrDecisionRecordNotOpen, http.StatusConflict, "decision_request_not_open", false},
	{store.ErrLedgerAckBackward, http.StatusConflict, "ledger_ack_backward", true},
	{store.ErrStatusUpdateInvalid, http.StatusBadRequest, "invalid_status_update", true},
	{store.ErrChangeNotFound, http.StatusNotFound, "not_found", false},
	{store.ErrChangeDependencyCycle, http.StatusConflict, "change_dependency_cycle", false},
	{store.ErrVerificationExists, http.StatusConflict, "verification_exists", false},
	{store.ErrInvalidChangeWrite, http.StatusBadRequest, "invalid_context", true},
	{store.ErrInvalidPlacement, http.StatusBadRequest, "invalid_context", true},
	{store.ErrDotsResultNotFound, http.StatusNotFound, "not_found", false},
	{store.ErrDotsAdoptionFinal, http.StatusConflict, "dots_adoption_final", false},
	{store.ErrInvalidDeployService, http.StatusBadRequest, "invalid_deploy_service", true},
	{store.ErrServiceNotFound, http.StatusNotFound, "not_found", false},
	{store.ErrDeployNotFound, http.StatusNotFound, "not_found", false},
	{store.ErrDeployAttemptConflict, http.StatusConflict, "deploy_attempt_conflict", false},
	{store.ErrDeployAttemptFinished, http.StatusConflict, "deploy_attempt_finished", false},
}

// appErr writes the response for an error returned by the store or a
// service. Sentinels go through errorRules. An infrastructure failure (the
// database or network, not the request) is logged and answered 500
// internal_error. Anything else is a rejected request: 400 invalid_context.
func appErr(w http.ResponseWriter, r *http.Request, e error) {
	var wrkErr wrkrecords.Error
	if errors.As(e, &wrkErr) {
		writeError(w, wrkrecords.Status(wrkErr), wrkrecords.Code(wrkErr))
		return
	}
	// These codes are the error's own text: fixed strings or store-built
	// validator messages, never echoed input.
	if errors.Is(e, attachments.ErrMIME) || errors.Is(e, attachments.ErrExtension) || errors.Is(e, store.ErrInvalidDecisionRequest) {
		writeError(w, http.StatusBadRequest, e.Error())
		return
	}
	var repConflict *store.BenchRepConflictError
	if errors.As(e, &repConflict) {
		jsonOut(w, http.StatusConflict, map[string]any{"error": "bench_rep_conflict", "conflicts": repConflict.Conflicts})
		return
	}
	var outcomeConflict *store.BenchRepOutcomeConflictError
	if errors.As(e, &outcomeConflict) {
		jsonOut(w, http.StatusConflict, map[string]any{"error": "bench_rep_outcome_conflict", "field": outcomeConflict.Field})
		return
	}
	var similar *store.SimilarTasksError
	if errors.As(e, &similar) {
		jsonOut(w, http.StatusConflict, map[string]any{"error": "similar_tasks", "similar_tasks": similar.Tasks})
		return
	}
	for _, rule := range errorRules {
		if !errors.Is(e, rule.err) {
			continue
		}
		body := map[string]string{"error": rule.code}
		if rule.reason {
			if reason, ok := strings.CutPrefix(e.Error(), rule.err.Error()+": "); ok && reason != "" {
				body["reason"] = reason
			}
		}
		jsonOut(w, rule.status, body)
		return
	}
	if p, ok := guard.SecretPattern(e); ok {
		jsonOut(w, http.StatusBadRequest, map[string]string{"error": "secret_like_content", "pattern": p})
		return
	}
	if isInternal(e) {
		writeInternal(w, r, "internal_error", e)
		return
	}
	writeError(w, http.StatusBadRequest, "invalid_context")
}

// writeInternal logs an infrastructure failure as one api.internal_error line
// and answers 500 with code. request_id ties the client's error to this log
// line and to Sentry. trace_id, present only for a sampled OTLP trace, joins
// the line and the error to that trace.
func writeInternal(w http.ResponseWriter, r *http.Request, code string, e error) {
	requestID := telemetry.RequestID(r.Context())
	traceID := telemetry.TraceID(r.Context())
	client, _ := store.WrkPrincipal(r.Context())
	log.Printf("api.internal_error request_id=%s trace_id=%s route=%q client=%q code=%s err=%q", requestID, traceID, r.Pattern, client, code, e.Error())
	body := map[string]string{"error": code}
	if requestID != "" {
		body["request_id"] = requestID
	}
	if traceID != "" {
		body["trace_id"] = traceID
	}
	jsonOut(w, http.StatusInternalServerError, body)
}

// isInternal reports whether e is a failure of the server's dependencies
// rather than of the request. Postgres data exceptions (class 22) and
// integrity violations (class 23) are caused by the request's values, so
// they stay client errors.
func isInternal(e error) bool {
	var pg *pgconn.PgError
	if errors.As(e, &pg) {
		class := pg.Code[:min(2, len(pg.Code))]
		return class != "22" && class != "23"
	}
	var connect *pgconn.ConnectError
	if errors.As(e, &connect) {
		return true
	}
	var netErr net.Error
	if errors.As(e, &netErr) {
		return true
	}
	return pgconn.Timeout(e) ||
		errors.Is(e, context.DeadlineExceeded) ||
		errors.Is(e, context.Canceled) ||
		errors.Is(e, pgx.ErrTxClosed)
}
