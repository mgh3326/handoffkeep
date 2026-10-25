package wrk

// Code is one stable error code of the design §4 map. An error response is
// exactly {"error": code}; nothing else from the request or the store is
// echoed.
type Code string

const (
	InvalidRequest       Code = "invalid_request"
	InvalidCursor        Code = "invalid_cursor"
	InvalidVerdict       Code = "invalid_verdict"
	InvalidPath          Code = "invalid_path"
	GrantBeforeCap       Code = "grant_before_cap"
	Unauthorized         Code = "unauthorized"
	ScopeDenied          Code = "scope_denied"
	HostMismatch         Code = "host_mismatch"
	NotFound             Code = "not_found"
	IdempotencyConflict  Code = "idempotency_conflict"
	SequenceConflict     Code = "sequence_conflict"
	ClaimConflict        Code = "claim_conflict"
	JobEnded             Code = "job_ended"
	ReviewSealed         Code = "review_sealed"
	RoundCap             Code = "round_cap"
	RoundNotOpen         Code = "round_not_open"
	RoundHasVerdict      Code = "round_has_verdict"
	RoundVoided          Code = "round_voided"
	HeadMismatch         Code = "head_mismatch"
	DuplicatePath        Code = "duplicate_path"
	DuplicateContent     Code = "duplicate_content"
	DuplicateHead        Code = "duplicate_head"
	GrantInvalid         Code = "grant_invalid"
	GrantConsumed        Code = "grant_consumed"
	GrantExpired         Code = "grant_expired"
	ZeroRounds           Code = "zero_rounds"
	ReviewIncomplete     Code = "review_incomplete"
	BlockerUnresolved    Code = "blocker_unresolved"
	WrkReceiptRequired   Code = "wrk_receipt_required"
	ImportUnsealed       Code = "import_unsealed"
	ImportSealed         Code = "import_sealed"
	MigrationFrozen      Code = "migration_frozen"
	RequestTooLarge      Code = "request_too_large"
	MethodNotAllowed     Code = "method_not_allowed"
	UnsupportedMediaType Code = "unsupported_media_type"
	Unavailable          Code = "wrk_unavailable"
	RecoveryRequired     Code = "wrk_recovery_required"
)

// Error carries a stable code through the core. Its text is the code only.
type Error struct{ Code Code }

func (e *Error) Error() string { return string(e.Code) }

// Fail returns the stable error for code.
func Fail(code Code) error { return &Error{Code: code} }

// statusByCode is the design §4 HTTP map.
var statusByCode = map[Code]int{
	InvalidRequest: 400, InvalidCursor: 400, InvalidVerdict: 400, InvalidPath: 400, GrantBeforeCap: 400,
	Unauthorized: 401,
	ScopeDenied:  403, HostMismatch: 403,
	NotFound:            404,
	IdempotencyConflict: 409, SequenceConflict: 409, ClaimConflict: 409, JobEnded: 409, ReviewSealed: 409,
	RoundCap: 409, RoundNotOpen: 409, RoundHasVerdict: 409, RoundVoided: 409, HeadMismatch: 409,
	DuplicatePath: 409, DuplicateContent: 409, DuplicateHead: 409, GrantInvalid: 409, GrantConsumed: 409,
	GrantExpired: 409, ZeroRounds: 409, ReviewIncomplete: 409, BlockerUnresolved: 409, WrkReceiptRequired: 409,
	ImportUnsealed: 409, ImportSealed: 409, MigrationFrozen: 409,
	RequestTooLarge:      413,
	MethodNotAllowed:     405,
	UnsupportedMediaType: 415,
	Unavailable:          503, RecoveryRequired: 503,
}

// Status is the HTTP status for a stable code; ok is false for a code
// outside the §4 map.
func Status(code Code) (int, bool) {
	s, ok := statusByCode[code]
	return s, ok
}

// Codes lists every server stable code.
func Codes() []Code {
	out := make([]Code, 0, len(statusByCode))
	for c := range statusByCode {
		out = append(out, c)
	}
	return out
}

// Local CLI/adapter codes of design §6b. They never come from the server.
const (
	CLIUsage               Code = "cli_usage"
	TransportUnavailable   Code = "transport_unavailable"
	TransportIndeterminate Code = "transport_indeterminate"
	ProtocolInvalid        Code = "protocol_invalid"
	O3Unsupported          Code = "o3_unsupported"
)

// exitByCode is the design §6b stable-code to process-exit map, limited to
// the codes the S1 CLI itself can emit (intent and history codes are C1's).
var exitByCode = map[Code]int{
	RoundCap:    78,
	Unavailable: 75, RecoveryRequired: 75,
	Unauthorized: 77, ScopeDenied: 77, HostMismatch: 77,
	NotFound:       66,
	InvalidRequest: 65, InvalidCursor: 65, InvalidVerdict: 65, InvalidPath: 65, GrantBeforeCap: 65,
	RequestTooLarge: 65, MethodNotAllowed: 65, UnsupportedMediaType: 65,
	IdempotencyConflict: 73, SequenceConflict: 73, ClaimConflict: 73, JobEnded: 73, ReviewSealed: 73,
	RoundNotOpen: 73, RoundHasVerdict: 73, RoundVoided: 73, HeadMismatch: 73, DuplicatePath: 73,
	DuplicateContent: 73, DuplicateHead: 73, GrantInvalid: 73, GrantConsumed: 73, GrantExpired: 73,
	ZeroRounds: 73, ReviewIncomplete: 73, BlockerUnresolved: 73, WrkReceiptRequired: 73, ImportUnsealed: 73,
	ImportSealed: 73, MigrationFrozen: 73,
	CLIUsage:             2,
	TransportUnavailable: 75, TransportIndeterminate: 75, ProtocolInvalid: 75, O3Unsupported: 75,
}

// Exit is the CLI process exit for code; ok is false for an unmapped code.
func Exit(code Code) (int, bool) {
	e, ok := exitByCode[code]
	return e, ok
}
