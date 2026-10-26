package wrkrecords

// LegacyIntent describes a mutation's actual effect, rather than the route or
// a caller's free-form By string. All entry points use this same predicate.
type LegacyIntent struct {
	TaskID                                    int64
	CurrentJob, JobID, To, Kind, Head, Digest string
	Binding                                   bool
	Receipt                                   *Receipt
}

func (s Snapshot) ProtectedTask(id int64) bool {
	for job, rs := range s.Jobs {
		if ShadowSubjectProtectionStartsAtActivate && s.Activation(job) == 0 {
			continue
		}
		for _, r := range rs {
			if r.Kind == "claim" && !Review(&r) && Num(r.Payload, "task_id") == id {
				return true
			}
		}
	}
	return false
}

func (s Snapshot) CheckLegacy(p *Policy, actor string, in LegacyIntent) error {
	protected := in.TaskID > 0 && s.ProtectedTask(in.TaskID)
	if in.JobID != "" && in.Kind == "" && s.ReceiptRequired(in.JobID) {
		protected = true
	}
	terminal := in.Kind == "job.completed" || in.Kind == "job.joined" || in.Kind == "job.revoked" || in.Kind == "job.reaped" || in.Kind == "job.handed_off" || in.Kind == "job.takeover" || in.Kind == "end"
	if terminal && s.ReceiptRequired(in.JobID) {
		protected = true
	}
	if !protected {
		return nil
	}
	if actor == "" {
		return Error("scope_denied")
	}
	required := func() error { return Error("wrk_receipt_required") }
	if in.Binding {
		return required()
	}
	receipt := in.Receipt
	if receipt == nil {
		return required()
	}
	job := in.CurrentJob
	if terminal {
		job = in.JobID
	}
	c := s.Claim(job)
	if c == nil || receipt.JobID != job {
		return required()
	}
	var record *Record
	for _, r := range s.Jobs[job] {
		if r.Seq == receipt.Seq {
			x := r
			record = &x
		}
	}
	if record == nil || record.RecordHash != receipt.Hash || record.Generation != receipt.Generation || record.ClaimSeq != c.Seq {
		return required()
	}
	// A shadow subject's observations never become protected completion
	// receipts on activation. Review-role lifecycle is required from claim.
	if !Review(c) && (record.Kind == "completed" || record.Kind == "joined") && record.Seq < s.Activation(job) {
		return required()
	}
	if terminal {
		if !p.Allows(actor, "relay.publish") {
			return Error("scope_denied")
		}
		if p.Host(actor) != Text(c.Payload, "host_client_id") {
			return Error("host_mismatch")
		}
		kind := in.Kind
		if kind == "job.completed" {
			kind = "completed"
		} else if kind == "job.joined" {
			kind = "joined"
		} else {
			kind = "end"
		}
		if record.Kind != kind {
			return required()
		}
		if ((kind == "joined" || kind == "completed") && in.Head != Text(record.Payload, "head")) || ((kind == "completed" || kind == "joined") && in.Digest != Text(record.Payload, "report_sha256")) {
			return required()
		}
		return nil
	}
	if !Central(actor) || !p.Allows(actor, "task.state") {
		return required()
	}
	if Review(c) || Num(c.Payload, "task_id") != in.TaskID {
		return required()
	}
	kind := "claim"
	if in.To == "join" || in.To == "merged" {
		kind = "joined"
	}
	if in.To == "dropped" {
		kind = "end"
	}
	if record.Kind != kind {
		return required()
	}
	return nil
}
