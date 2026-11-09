package ytwrite

import "time"

// StatusOf is the three-way answer for a lifecycle state. Only applied is
// done: remote_unknown, like prepared and dispatched, is pending (hk 3667 Q3).
func StatusOf(s State) Status {
	return ""
}

// OpView is the read-only shape of one op that the operator's view (/v1/yt/ops,
// `handoffkeep yt ops`) shows. It carries no payload: the body stays in PG.
type OpView struct {
	ID            int64      `json:"id"`
	Kind          string     `json:"kind"`
	EntityKind    string     `json:"entity_kind"`
	EntityID      string     `json:"entity_id"`
	YTTargetID    string     `json:"yt_target_id"`
	Actor         string     `json:"actor"`
	State         State      `json:"state"`
	Status        Status     `json:"status"`
	Reason        string     `json:"reason,omitempty"`
	YTResultID    string     `json:"yt_result_id,omitempty"`
	Attempts      int        `json:"attempts"`
	Reconciles    int        `json:"reconciles"`
	CreatedAt     time.Time  `json:"created_at"`
	NextAttemptAt time.Time  `json:"next_attempt_at"`
	ResolvedAt    *time.Time `json:"resolved_at,omitempty"`
}

// ViewOf renders an op for the operator's view.
func ViewOf(op Op) OpView {
	return OpView{}
}
