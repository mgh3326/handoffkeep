package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/youtrack"
)

// `youtrack decisions-backfill` (hk 4054): bind each decision's imported
// DEC-<id> issue, or the issue the legacy `youtrack sync` mirrored it to,
// after verifying it (hk 3833 §2.1), so its decision_fields
// snapshots have a target and its full text reaches the issue. Read-only by
// default (the preflight); --apply binds.

const youtrackDecisionsBackfillUsage = "usage: youtrack decisions-backfill [--apply] [--by NAME] [--limit N]"

// ytDecisionsListMax caps the decision ids listed per class in the report.
const ytDecisionsListMax = 20

// ytDecisionsClass is one class of the decisions-backfill report.
type ytDecisionsClass struct {
	Count int     `json:"count"`
	IDs   []int64 `json:"ids"`
}

func (c *ytDecisionsClass) add(id int64) {
	c.Count++
	if len(c.IDs) < ytDecisionsListMax {
		c.IDs = append(c.IDs, id)
	}
}

// ytDecisionsMismatch is why one decision's issue did not verify.
type ytDecisionsMismatch struct {
	ID     int64  `json:"id"`
	Reason string `json:"reason"`
}

// ytDecisionsReport is what one run found. Verified: the DEC-<id> issue, or
// the legacy mirror's issue (also counted in Legacy), verifies (in --apply,
// it was bound now); Bound: --apply published the
// binding; Missing: no DEC-<id> issue; Mismatch: an issue that fails the
// verification (never bound); PendingOp: the decision's decision_create op is
// unresolved (the op gives it its issue); Conflict: the binding was refused
// (the decision or the issue is bound elsewhere); Error: the read failed, run
// again.
type ytDecisionsReport struct {
	Tenant     string                `json:"tenant"`
	ProjectID  string                `json:"project_id"`
	Apply      bool                  `json:"apply"`
	Snapshots  string                `json:"snapshots"`
	Checked    int                   `json:"checked"`
	Verified   ytDecisionsClass      `json:"verified"`
	Legacy     ytDecisionsClass      `json:"verified_legacy"`
	Bound      ytDecisionsClass      `json:"bound"`
	Missing    ytDecisionsClass      `json:"missing"`
	Mismatch   ytDecisionsClass      `json:"mismatch"`
	Reasons    []ytDecisionsMismatch `json:"mismatch_reasons"`
	PendingOp  ytDecisionsClass      `json:"pending_op"`
	Conflict   ytDecisionsClass      `json:"conflict"`
	Error      ytDecisionsClass      `json:"error"`
	errorTexts []string
}

// ytDecisionIssueGetter reads one issue by id (WriteRemote.GetIssue).
type ytDecisionIssueGetter interface {
	GetIssue(ctx context.Context, issueID string) (youtrack.Issue, error)
}

// ytDecisionsBackfillStore is the store surface of the backfill.
type ytDecisionsBackfillStore interface {
	UnboundYTDecisions(ctx context.Context, afterID int64, limit int) ([]store.YTDecisionCandidate, error)
	BindYTDecision(ctx context.Context, b store.YTDecisionBinding, now time.Time) (bool, error)
	ListYouTrackDecisionMirrors(ctx context.Context, decisionIDs []int64) (map[int64]store.YouTrackDecisionMirror, error)
}

type ytDecisionsOptions struct {
	Tenant    string
	ProjectID string
	Apply     bool
	BindBy    string
	// Limit bounds the decisions checked (0: all).
	Limit int
	// Snapshots is what a binding prepares: "on" (the decisions mode is
	// on, the first snapshot is prepared with it) or "off".
	Snapshots string
	Now       func() time.Time
}

// ytDecisionsBackfillRun checks every unbound decision, oldest first: one GET
// of DEC-<id> each, then youtrack.VerifyDecisionIssue; when that issue is
// missing or does not verify and the legacy mirror recorded another issue for
// the decision, one GET of that one, verified without the readable-id rule. With Apply, a
// verified issue is bound (store.BindYTDecision). A failed read is counted
// and the run goes on.
func ytDecisionsBackfillRun(ctx context.Context, st ytDecisionsBackfillStore, issues ytDecisionIssueGetter, o ytDecisionsOptions) (ytDecisionsReport, error) {
	r := ytDecisionsReport{Tenant: o.Tenant, ProjectID: o.ProjectID, Apply: o.Apply, Snapshots: o.Snapshots, Reasons: []ytDecisionsMismatch{}}
	now := o.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	var after int64
	for {
		page, err := st.UnboundYTDecisions(ctx, after, 200)
		if err != nil {
			return r, err
		}
		ids := make([]int64, 0, len(page))
		for _, c := range page {
			ids = append(ids, c.ID)
		}
		mirrors, err := st.ListYouTrackDecisionMirrors(ctx, ids)
		if err != nil {
			return r, err
		}
		for _, c := range page {
			after = c.ID
			if o.Limit > 0 && r.Checked >= o.Limit {
				return r, nil
			}
			r.Checked++
			if c.PendingOp {
				r.PendingOp.add(c.ID)
				continue
			}
			// The import's DEC-<id> first; then the issue the legacy
			// `youtrack sync` recorded for the decision
			// (youtrack_decision_mirrors), whose number YouTrack chose.
			issue, class, reason, err := ytDecisionIssueVerify(ctx, issues, youtrack.DecisionReadable(c.ID), c.ID, o.ProjectID, youtrack.DecisionReadable(c.ID))
			if m, ok := mirrors[c.ID]; ok && class != "verified" && class != "error" && m.IssueID != "" && m.IssueID != issue.ID {
				if li, lclass, lreason, lerr := ytDecisionIssueVerify(ctx, issues, m.IssueID, c.ID, o.ProjectID, ""); lclass == "verified" || lclass == "error" {
					issue, class, reason, err = li, lclass, lreason, lerr
					if lclass == "verified" {
						r.Legacy.add(c.ID)
					}
				}
			}
			switch class {
			case "missing":
				r.Missing.add(c.ID)
				continue
			case "error":
				r.Error.add(c.ID)
				r.errorTexts = append(r.errorTexts, fmt.Sprintf("decision %d: %v", c.ID, err))
				continue
			case "mismatch":
				r.Mismatch.add(c.ID)
				if len(r.Reasons) < ytDecisionsListMax {
					r.Reasons = append(r.Reasons, ytDecisionsMismatch{ID: c.ID, Reason: reason})
				}
				continue
			}
			r.Verified.add(c.ID)
			if !o.Apply {
				continue
			}
			created, err := st.BindYTDecision(ctx, store.YTDecisionBinding{DecisionID: c.ID, Tenant: o.Tenant, IssueID: issue.ID,
				IssueReadable: issue.IDReadable, ProjectID: o.ProjectID, BoundBy: o.BindBy}, now())
			switch {
			case errors.Is(err, store.ErrYTDecisionOpPending):
				r.PendingOp.add(c.ID)
			case errors.Is(err, store.ErrYTDecisionBindingConflict):
				r.Conflict.add(c.ID)
			case err != nil:
				return r, fmt.Errorf("bind decision %d: %w", c.ID, err)
			case created:
				r.Bound.add(c.ID)
			}
		}
		if len(page) < 200 {
			return r, nil
		}
	}
}

// ytDecisionIssueVerify reads one issue and classifies it for decision id:
// "verified", "missing" (404), "mismatch" (with the reason) or "error" (the
// read failed).
func ytDecisionIssueVerify(ctx context.Context, issues ytDecisionIssueGetter, ref string, id int64, projectID, wantReadable string) (youtrack.Issue, string, string, error) {
	gctx, cancel := context.WithTimeout(ctx, ytCommentsGetTimeout)
	issue, err := issues.GetIssue(gctx, ref)
	cancel()
	switch {
	case errors.Is(err, youtrack.ErrNotFound):
		return youtrack.Issue{}, "missing", "", nil
	case err != nil:
		return youtrack.Issue{}, "error", "", err
	}
	if reason := youtrack.VerifyDecisionIssue(issue, id, projectID, wantReadable); reason != "" {
		return issue, "mismatch", reason, nil
	}
	return issue, "verified", "", nil
}

// youtrackDecisionsBackfillCmd is `youtrack decisions-backfill`. Without
// --apply it is the preflight: it opens the database read-only and never
// migrates, and writes nothing anywhere. With --apply it migrates as the
// server does and binds every verified issue; in HANDOFFKEEP_YT_DECISIONS_MODE
// =on each new binding also prepares the decision's first snapshot (its full
// text), otherwise the text reaches the issue with the decision's next change.
// Both read YouTrack only (the DEC project's id, then one GET per decision).
func youtrackDecisionsBackfillCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("youtrack decisions-backfill", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	apply := fs.Bool("apply", false, "bind the verified issues (default: report only)")
	by := fs.String("by", "operator:cli", "who bound the decisions")
	limit := fs.Int("limit", 0, "check at most N decisions (0: all)")
	ordered, err := splitArgs(args, "by", "limit")
	if err != nil {
		return err
	}
	if err := fs.Parse(ordered); err != nil {
		return fmt.Errorf("%v; %s", err, youtrackDecisionsBackfillUsage)
	}
	bindBy := strings.TrimSpace(*by)
	if fs.NArg() != 0 || *limit < 0 || bindBy == "" {
		return errors.New(youtrackDecisionsBackfillUsage)
	}
	mode, err := ytDecisionsModeFromEnv()
	if err != nil {
		return exitCodeError{code: youtrackExitConfig, err: err}
	}
	dbURL := strings.TrimSpace(os.Getenv("HANDOFFKEEP_DB_URL"))
	baseURL := strings.TrimSpace(os.Getenv("YOUTRACK_URL"))
	token := strings.TrimSpace(os.Getenv("YOUTRACK_BOT_API_KEY"))
	if dbURL == "" || baseURL == "" || token == "" {
		return exitCodeError{code: youtrackExitConfig, err: errors.New("HANDOFFKEEP_DB_URL, YOUTRACK_URL and YOUTRACK_BOT_API_KEY are required")}
	}
	client, err := youtrack.New(youtrack.Config{BaseURL: baseURL, Token: token, UserAgent: "handoffkeep-ytwrite"})
	if err != nil {
		return exitCodeError{code: youtrackExitConfig, err: err}
	}
	remote := youtrack.NewWriteRemote(client)
	openCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	openStore := store.OpenReadOnly
	if *apply {
		openStore = store.Open
	}
	st, err := openStore(openCtx, dbURL)
	if err != nil {
		return err
	}
	defer st.Close()
	if !*apply {
		applied, err := st.MigrationApplied(openCtx, store.YTDecisionBindingsMigration)
		if err != nil {
			return err
		}
		if !applied {
			return exitCodeError{code: youtrackExitConfig, err: fmt.Errorf("the database lacks migration %s: deploy the release that adds it first; the preflight never migrates", store.YTDecisionBindingsMigration)}
		}
	}
	projectCtx, cancelProject := context.WithTimeout(context.Background(), ytCommentsGetTimeout)
	projectID, err := client.ProjectID(projectCtx, youtrack.DecisionProject)
	cancelProject()
	if err != nil {
		return exitCodeError{code: youtrackExitFailed, err: fmt.Errorf("read the %s project: %w", youtrack.DecisionProject, err)}
	}
	tenant := remote.Tenant()
	snapshots := string(store.YTDecisionsOff)
	if *apply && mode == store.YTDecisionsOn {
		st.SetYTDecisionsMode(store.YTDecisionsOn, tenant, projectID, youtrack.DecisionProject)
		snapshots = string(store.YTDecisionsOn)
	}
	report, err := ytDecisionsBackfillRun(context.Background(), st, remote, ytDecisionsOptions{Tenant: tenant, ProjectID: projectID,
		Apply: *apply, BindBy: bindBy, Limit: *limit, Snapshots: snapshots})
	if err != nil {
		return err
	}
	if err := printJSON(out, report); err != nil {
		return err
	}
	if report.Error.Count > 0 {
		for _, line := range report.errorTexts {
			fmt.Fprintln(os.Stderr, line)
		}
		return exitCodeError{code: youtrackExitFailed, err: fmt.Errorf("%d decisions could not be read (class error); run again", report.Error.Count)}
	}
	return nil
}
