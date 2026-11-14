package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/remote"
	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/youtrack"
	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// Slice b1a (hk 3739) and mode on (hk 3810): a new hk comment on a canary
// task, or in mode on on a task bound to its issue, also gets a
// protocol-tracked copy on its YouTrack issue. docs/youtrack-write-protocol.md.

const (
	// ytCommentsInterval is how often the worker runs one Drive pass.
	ytCommentsInterval = 5 * time.Second
	// ytCommentsDispatchTimeout bounds one YouTrack call (and one evidence
	// search); ytCommentsSettleTimeout bounds recording its outcome. When
	// serve stops, a call in flight finishes or times out and its settlement
	// gets at most ytCommentsSettleTimeout, so the worker returns within
	// their sum, under serve's worker stop budget (workerStopBudget, 5s), and
	// before the store closes. A call that times out becomes remote_unknown;
	// a settlement that cannot finish in time (a locked row, a slow database)
	// leaves the op dispatched, durable and unrecorded: the next worker's
	// stale recovery makes it remote_unknown and the marker search applies it
	// without a second POST.
	ytCommentsDispatchTimeout = 3 * time.Second
	ytCommentsSettleTimeout   = 1 * time.Second
	ytCommentsPassLimit       = 50
	// ytHeldRecoveryInterval is how often the worker runs the held-op
	// recovery pass (store.RecoverHeldYTOps); the pass only closes ops that
	// have been held for store.YTHeldRecoveryGrace.
	ytHeldRecoveryInterval = 5 * time.Minute
	// ytCommentsCatchUpLimit bounds the comments one catch-up pass prepares
	// ops for (store.CatchUpYTComments), oldest first.
	ytCommentsCatchUpLimit = 200
	// ytCommentsRequeueLimit bounds the comments one requeue pass copies
	// again (store.RequeueYTMovedComments), oldest first.
	ytCommentsRequeueLimit = 200
	// ytFeedPassLimit bounds the sync_log rows each feed consumer reads in
	// one worker pass (store.RunYTFeedConsumers).
	ytFeedPassLimit = 200
	// ytBindInterval is how often the worker runs the automatic binder pass
	// (HANDOFFKEEP_YT_BIND=on); ytBindScanLimit is how many new comments it
	// reads from the stream and ytBindPassLimit how many intents it binds or
	// refuses in one pass.
	ytBindInterval  = 10 * time.Minute
	ytBindScanLimit = 200
	ytBindPassLimit = 20
	// ytBindActor is who an automatic binding is attributed to in its history
	// row: the pass runs inside serve, not for a person.
	ytBindActor = "operator:auto-bind"
	// ytBindCopyMax is the most missing comments one automatic publication
	// copies in its own transaction (store.YTAutoBindCopyMax); a larger
	// history is refused with history_large before anything is written.
	ytBindCopyMax = store.YTAutoBindCopyMax
)

// ytCommentsModeFromEnv reads HANDOFFKEEP_YT_COMMENTS_MODE: unset or "off"
// (the default), "canary" or "on", trimmed and exactly in lower case. Any
// other value is an error, so a typo fails closed instead of guessing a mode.
func ytCommentsModeFromEnv() (store.YTCommentsMode, error) {
	switch raw := strings.TrimSpace(os.Getenv("HANDOFFKEEP_YT_COMMENTS_MODE")); raw {
	case "", string(store.YTCommentsOff):
		return store.YTCommentsOff, nil
	case string(store.YTCommentsCanary):
		return store.YTCommentsCanary, nil
	case string(store.YTCommentsOn):
		return store.YTCommentsOn, nil
	default:
		return "", fmt.Errorf("HANDOFFKEEP_YT_COMMENTS_MODE=%q: want off, canary or on", raw)
	}
}

// ytIssuesModeFromEnv reads HANDOFFKEEP_YT_ISSUES_MODE (hk 3898): unset or
// "off" (the default) or "on", trimmed and exactly in lower case like the
// comments mode. Any other value is an error.
func ytIssuesModeFromEnv() (store.YTIssuesMode, error) {
	switch raw := strings.TrimSpace(os.Getenv("HANDOFFKEEP_YT_ISSUES_MODE")); raw {
	case "", string(store.YTIssuesOff):
		return store.YTIssuesOff, nil
	case string(store.YTIssuesOn):
		return store.YTIssuesOn, nil
	default:
		return "", fmt.Errorf("HANDOFFKEEP_YT_ISSUES_MODE=%q: want off or on", raw)
	}
}

// ytFieldsModeFromEnv reads HANDOFFKEEP_YT_FIELDS_MODE (hk 4003): unset or
// "off" (the default) or "on", exactly like the issues mode. Any other value
// is an error.
func ytFieldsModeFromEnv() (store.YTFieldsMode, error) {
	switch raw := strings.TrimSpace(os.Getenv("HANDOFFKEEP_YT_FIELDS_MODE")); raw {
	case "", string(store.YTFieldsOff):
		return store.YTFieldsOff, nil
	case string(store.YTFieldsOn):
		return store.YTFieldsOn, nil
	default:
		return "", fmt.Errorf("HANDOFFKEEP_YT_FIELDS_MODE=%q: want off or on", raw)
	}
}

// ytMovesModeFromEnv reads HANDOFFKEEP_YT_MOVES_MODE (hk 4111): unset or "off"
// (the default) or "on", exactly like the fields mode. Any other value is an
// error.
func ytMovesModeFromEnv() (store.YTMovesMode, error) {
	switch raw := strings.TrimSpace(os.Getenv("HANDOFFKEEP_YT_MOVES_MODE")); raw {
	case "", string(store.YTMovesOff):
		return store.YTMovesOff, nil
	case string(store.YTMovesOn):
		return store.YTMovesOn, nil
	default:
		return "", fmt.Errorf("HANDOFFKEEP_YT_MOVES_MODE=%q: want off or on", raw)
	}
}

// ytDocCommentsModeFromEnv reads HANDOFFKEEP_YT_DOC_COMMENTS_MODE (hk 4xxx):
// unset or "off" (the default) or "on", exactly like the fields mode. Any
// other value is an error.
func ytDocCommentsModeFromEnv() (store.YTDocCommentsMode, error) {
	switch raw := strings.TrimSpace(os.Getenv("HANDOFFKEEP_YT_DOC_COMMENTS_MODE")); raw {
	case "", string(store.YTDocCommentsOff):
		return store.YTDocCommentsOff, nil
	case string(store.YTDocCommentsOn):
		return store.YTDocCommentsOn, nil
	default:
		return "", fmt.Errorf("HANDOFFKEEP_YT_DOC_COMMENTS_MODE=%q: want off or on", raw)
	}
}

// ytDecisionsModeFromEnv reads HANDOFFKEEP_YT_DECISIONS_MODE (hk 4054): unset
// or "off" (the default) or "on", exactly like the issues mode. Any other
// value is an error.
func ytDecisionsModeFromEnv() (store.YTDecisionsMode, error) {
	switch raw := strings.TrimSpace(os.Getenv("HANDOFFKEEP_YT_DECISIONS_MODE")); raw {
	case "", string(store.YTDecisionsOff):
		return store.YTDecisionsOff, nil
	case string(store.YTDecisionsOn):
		return store.YTDecisionsOn, nil
	default:
		return "", fmt.Errorf("HANDOFFKEEP_YT_DECISIONS_MODE=%q: want off or on", raw)
	}
}

// ytDocsModeFromEnv reads HANDOFFKEEP_YT_DOCS_MODE (hk 4206): unset or "off"
// (the default) or "on", exactly like the issues mode. Any other value is an
// error.
func ytDocsModeFromEnv() (store.YTDocsMode, error) {
	switch raw := strings.TrimSpace(os.Getenv("HANDOFFKEEP_YT_DOCS_MODE")); raw {
	case "", string(store.YTDocsOff):
		return store.YTDocsOff, nil
	case string(store.YTDocsOn):
		return store.YTDocsOn, nil
	default:
		return "", fmt.Errorf("HANDOFFKEEP_YT_DOCS_MODE=%q: want off or on", raw)
	}
}

// ytDocsSetting is the document-article mode and, when on, the YouTrack
// projects of the /shared and /unassigned buckets.
type ytDocsSetting struct {
	mode    store.YTDocsMode
	buckets map[string]store.YTArticleProject
}

// ytDecisionsProjectTimeout bounds the one YouTrack read serve makes at start
// in decisions mode on: the DEC project's id, which every decision_create op
// targets (the feed calls no YouTrack).
const ytDecisionsProjectTimeout = 10 * time.Second

// ytDecisionsSetting is the decisions mode and, when on, the DEC project's id.
type ytDecisionsSetting struct {
	mode      store.YTDecisionsMode
	projectID string
}

// ytCommentsCatchUpFromEnv reads HANDOFFKEEP_YT_COMMENTS_CATCHUP: unset or
// "on" (the default) runs the catch-up before each Drive pass in mode on,
// "off" runs only the issue-create catch-up (hk 3914). Any other value is an
// error, as for the mode. The default flipped with the removals (hk 4426):
// the legacy comment push that could post a comment and end without linking
// it no longer exists, so a comment with no op would otherwise stay in PG
// only.
func ytCommentsCatchUpFromEnv() (bool, error) {
	switch raw := strings.TrimSpace(os.Getenv("HANDOFFKEEP_YT_COMMENTS_CATCHUP")); raw {
	case "", "on":
		return true, nil
	case "off":
		return false, nil
	default:
		return false, fmt.Errorf("HANDOFFKEEP_YT_COMMENTS_CATCHUP=%q: want off or on", raw)
	}
}

// ytBindModeFromEnv reads HANDOFFKEEP_YT_BIND (hk 4492): unset or "off" (the
// default) or "on", trimmed and exactly in lower case like the modes. On makes
// the comment worker bind the tasks whose comments it sees to an issue that
// already verifies. Any other value is an error, so a typo fails closed.
func ytBindModeFromEnv() (bool, error) {
	switch raw := strings.TrimSpace(os.Getenv("HANDOFFKEEP_YT_BIND")); raw {
	case "", "off":
		return false, nil
	case "on":
		return true, nil
	default:
		return false, fmt.Errorf("HANDOFFKEEP_YT_BIND=%q: want off or on", raw)
	}
}

// ytCommentsWorkerFromEnv sets the store's gates and returns the write
// dispatcher for serve's worker list, or nil when the comments, issues,
// fields, document-comment, decisions and document-article modes are all off
// (which needs no YouTrack configuration). Moves mode on with fields mode off is
// a startup error: nothing would prepare or send a move. Any on without YOUTRACK_URL and
// YOUTRACK_BOT_API_KEY is a startup error, never a writer that silently does
// nothing; decisions mode on also reads the DEC project's id once.
func ytCommentsWorkerFromEnv(st *store.Store) (backgroundWorker, error) {
	mode, err := ytCommentsModeFromEnv()
	if err != nil {
		return nil, err
	}
	issues, err := ytIssuesModeFromEnv()
	if err != nil {
		return nil, err
	}
	fields, err := ytFieldsModeFromEnv()
	if err != nil {
		return nil, err
	}
	docs, err := ytDocCommentsModeFromEnv()
	if err != nil {
		return nil, err
	}
	decisions, err := ytDecisionsModeFromEnv()
	if err != nil {
		return nil, err
	}
	moves, err := ytMovesModeFromEnv()
	if err != nil {
		return nil, err
	}
	articles, err := ytDocsModeFromEnv()
	if err != nil {
		return nil, err
	}
	catchUp, err := ytCommentsCatchUpFromEnv()
	if err != nil {
		return nil, err
	}
	bind, err := ytBindModeFromEnv()
	if err != nil {
		return nil, err
	}
	// The binder publishes bindings the comment enqueue then reads, so it
	// needs the comment gate on: with the gate off it would bind tasks whose
	// comments nothing copies.
	if bind && mode != store.YTCommentsOn {
		return nil, fmt.Errorf("HANDOFFKEEP_YT_BIND=on needs HANDOFFKEEP_YT_COMMENTS_MODE=on; got HANDOFFKEEP_YT_COMMENTS_MODE=%s", mode)
	}
	// Moves are prepared only by the task_fields consumer and their
	// snapshots ride the fields gate, so moves on with fields off would start
	// no worker and prepare nothing: refuse it rather than look enabled
	// (hk 4117).
	if moves == store.YTMovesOn && fields != store.YTFieldsOn {
		return nil, fmt.Errorf("HANDOFFKEEP_YT_MOVES_MODE=on needs HANDOFFKEEP_YT_FIELDS_MODE=on (moves are prepared by the task fields feed); got HANDOFFKEEP_YT_FIELDS_MODE=%s", fields)
	}
	if st != nil {
		st.SetYTMovesMode(moves)
	}
	if mode == store.YTCommentsOff && issues == store.YTIssuesOff && fields == store.YTFieldsOff && docs == store.YTDocCommentsOff && decisions == store.YTDecisionsOff && articles == store.YTDocsOff {
		if st != nil {
			st.SetYTCommentsMode(store.YTCommentsOff)
			st.SetYTIssuesMode(store.YTIssuesOff, "")
			st.SetYTFieldsMode(store.YTFieldsOff, "")
			st.SetYTDocCommentsMode(store.YTDocCommentsOff)
			st.SetYTDecisionsMode(store.YTDecisionsOff, "", "", "")
			st.SetYTDocsMode(store.YTDocsOff, "", nil)
			st.SetYTMovesMode(store.YTMovesOff)
		}
		return nil, nil
	}
	which := "HANDOFFKEEP_YT_COMMENTS_MODE=" + string(mode)
	switch {
	case mode != store.YTCommentsOff:
	case issues != store.YTIssuesOff:
		which = "HANDOFFKEEP_YT_ISSUES_MODE=" + string(issues)
	case fields != store.YTFieldsOff:
		which = "HANDOFFKEEP_YT_FIELDS_MODE=" + string(fields)
	case docs != store.YTDocCommentsOff:
		which = "HANDOFFKEEP_YT_DOC_COMMENTS_MODE=" + string(docs)
	case decisions != store.YTDecisionsOff:
		which = "HANDOFFKEEP_YT_DECISIONS_MODE=" + string(decisions)
	default:
		which = "HANDOFFKEEP_YT_DOCS_MODE=" + string(articles)
	}
	baseURL := strings.TrimSpace(os.Getenv("YOUTRACK_URL"))
	token := strings.TrimSpace(os.Getenv("YOUTRACK_BOT_API_KEY"))
	if baseURL == "" || token == "" {
		return nil, fmt.Errorf("%s needs YOUTRACK_URL and YOUTRACK_BOT_API_KEY", which)
	}
	if st == nil {
		return nil, fmt.Errorf("%s needs the store", which)
	}
	client, err := youtrack.New(youtrack.Config{BaseURL: baseURL, Token: token, UserAgent: "handoffkeep-ytwrite"})
	if err != nil {
		return nil, err
	}
	remote := youtrack.NewWriteRemote(client)
	dec := ytDecisionsSetting{mode: decisions}
	if decisions == store.YTDecisionsOn {
		// A decision_create op targets the DEC project by id, which only
		// YouTrack knows; the feed that prepares the op calls no YouTrack,
		// so the id is read once here. A failure stops the start, never a
		// mode that silently prepares nothing.
		ctx, cancel := context.WithTimeout(context.Background(), ytDecisionsProjectTimeout)
		dec.projectID, err = client.ProjectID(ctx, youtrack.DecisionProject)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("%s: read the %s project: %w", "HANDOFFKEEP_YT_DECISIONS_MODE=on", youtrack.DecisionProject, err)
		}
	}
	art := ytDocsSetting{mode: articles}
	if articles == store.YTDocsOn {
		// A new document's article goes to the import's /shared or
		// /unassigned project when it has no task or its task no project;
		// only YouTrack knows their ids, so they are read once here, as the
		// articles backfill does (by name). A failure stops the start.
		ctx, cancel := context.WithTimeout(context.Background(), ytDecisionsProjectTimeout)
		buckets, err := ytArticleBuckets(ctx, client)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("HANDOFFKEEP_YT_DOCS_MODE=on: read the article bucket projects: %w", err)
		}
		art.buckets = map[string]store.YTArticleProject{}
		for k, b := range buckets {
			art.buckets[k] = store.YTArticleProject{ID: b.ID, ShortName: b.ShortName}
		}
	}
	var issueBucketID string
	if issues == store.YTIssuesOn || bind {
		ctx, cancel := context.WithTimeout(context.Background(), ytCommentsGetTimeout)
		issueBucketID, err = youtrack.ResolveBucketProject(ctx, client)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("%s: resolve the %s bucket project: %w", which, youtrack.BucketShortName, err)
		}
	}
	st.SetYTIssuesBucketProject(issueBucketID)
	w, err := newYTWriteWorker(st, remote, remote.Tenant(), mode, issues, fields, docs, dec, art, catchUp, bind, issueBucketID, nil)
	if err != nil {
		return nil, err
	}
	return w, nil
}

// newYTWriteWorker sets the gates (the comments mode may be off when the
// issues, fields, document-comment or decisions mode is on) and builds the
// dispatcher for one tenant over remote. catchUp applies to the comments mode
// only; bind (HANDOFFKEEP_YT_BIND) runs the automatic binder in that same
// worker, verifying no-project tasks against bucketID.
func newYTWriteWorker(st *store.Store, remote ytwrite.Remote, tenant string, comments store.YTCommentsMode, issues store.YTIssuesMode, fields store.YTFieldsMode, docs store.YTDocCommentsMode, decisions ytDecisionsSetting, articles ytDocsSetting, catchUp, bind bool, bucketID string, now func() time.Time) (ytCommentsWorker, error) {
	if comments == store.YTCommentsOff && issues != store.YTIssuesOn && fields != store.YTFieldsOn && docs != store.YTDocCommentsOn && decisions.mode != store.YTDecisionsOn && articles.mode != store.YTDocsOn {
		return ytCommentsWorker{}, errors.New("the write worker runs with the comments, issues, fields, document-comment, decisions or docs mode on")
	}
	st.SetYTIssuesMode(issues, tenant)
	log.Printf("ytwrite.issues_mode mode=%s tenant=%q", issues, tenant)
	st.SetYTFieldsMode(fields, tenant)
	log.Printf("ytwrite.fields_mode mode=%s tenant=%q", fields, tenant)
	st.SetYTDocCommentsMode(docs)
	log.Printf("ytwrite.doc_comments_mode mode=%s tenant=%q", docs, tenant)
	st.SetYTDecisionsMode(decisions.mode, tenant, decisions.projectID, youtrack.DecisionProject)
	log.Printf("ytwrite.decisions_mode mode=%s tenant=%q project_id=%q", decisions.mode, tenant, decisions.projectID)
	st.SetYTDocsMode(articles.mode, tenant, articles.buckets)
	log.Printf("ytwrite.docs_mode mode=%s tenant=%q shared=%q unassigned=%q", articles.mode, tenant,
		articles.buckets[store.YTArticleBucketShared].ID, articles.buckets[store.YTArticleBucketUnassigned].ID)
	if comments != store.YTCommentsOff {
		return newYTCommentsWorker(st, remote, tenant, comments, catchUp, bind, bucketID, now)
	}
	p, err := ytwrite.New(st.YTCommentConfig(tenant, ytwrite.Config{
		Remote:          remote,
		Now:             now,
		DispatchTimeout: ytCommentsDispatchTimeout,
		SettleTimeout:   ytCommentsSettleTimeout,
	}))
	if err != nil {
		return ytCommentsWorker{}, err
	}
	st.SetYTCommentsMode(store.YTCommentsOff)
	return ytCommentsWorker{protocol: p, feed: st, intents: st, recovery: st, tenant: tenant}, nil
}

// newYTCommentsWorker sets the gate to mode (canary or on) and builds the
// dispatcher for one tenant over remote; catchUp runs the catch-up before
// each Drive pass (HANDOFFKEEP_YT_COMMENTS_CATCHUP), and bind
// (HANDOFFKEEP_YT_BIND) the automatic binder. now is the protocol clock (nil:
// the wall clock).
func newYTCommentsWorker(st *store.Store, remote ytwrite.Remote, tenant string, mode store.YTCommentsMode, catchUp, bind bool, bucketID string, now func() time.Time) (ytCommentsWorker, error) {
	if mode != store.YTCommentsCanary && mode != store.YTCommentsOn {
		return ytCommentsWorker{}, fmt.Errorf("the comment worker runs in mode canary or on, not %q", mode)
	}
	p, err := ytwrite.New(st.YTCommentConfig(tenant, ytwrite.Config{
		Remote:          remote,
		Now:             now,
		DispatchTimeout: ytCommentsDispatchTimeout,
		SettleTimeout:   ytCommentsSettleTimeout,
	}))
	if err != nil {
		return ytCommentsWorker{}, err
	}
	st.SetYTCommentsMode(mode)
	log.Printf("ytwrite.comments_enabled mode=%s tenant=%q catchup=%t", mode, tenant, catchUp)
	w := ytCommentsWorker{protocol: p, feed: st, intents: st, recovery: st, tenant: tenant}
	if mode == store.YTCommentsOn {
		// Only mode on queues a comment behind its task's issue_create, so
		// only mode on has queued comments to copy again (hk 4569).
		w.requeue = st
	}
	if bind {
		b, err := newYTBindingBinder(st, remote, tenant, bucketID)
		if err != nil {
			return ytCommentsWorker{}, err
		}
		log.Printf("ytwrite.bind_mode mode=on tenant=%q", tenant)
		w.binder = b
	}
	if catchUp {
		w.catchUp = st
	} else {
		w.catchUp = ytIssueCommentCatchUp{st}
	}
	return w, nil
}

// ytCommentCatchUpper prepares the ops of comments the enqueue missed
// (store.CatchUpYTComments).
type ytCommentCatchUpper interface {
	CatchUpYTComments(ctx context.Context, limit int, skip []int64) (store.YTCatchUp, error)
}

// ytIssueCommentCatchUp is the catch-up limited to the bindings issue_create
// ops published (store.CatchUpYTIssueComments, hk 3914): the worker's
// catch-up when HANDOFFKEEP_YT_COMMENTS_CATCHUP is off.
type ytIssueCommentCatchUp struct{ st *store.Store }

func (c ytIssueCommentCatchUp) CatchUpYTComments(ctx context.Context, limit int, skip []int64) (store.YTCatchUp, error) {
	return c.st.CatchUpYTIssueComments(ctx, limit, skip)
}

// ytCommentRequeuer copies again the queued comments rejected at their claim
// because the binding disagreed, once it agrees (store.RequeueYTMovedComments,
// hk 4569).
type ytCommentRequeuer interface {
	RequeueYTMovedComments(ctx context.Context, tenant string, limit int) ([]store.YTRequeued, error)
}

// ytHeldRecoverer closes the prepared ops whose task lost the binding that
// admitted them (store.RecoverHeldYTOps, hk 4491).
type ytHeldRecoverer interface {
	RecoverHeldYTOps(ctx context.Context, tenant string, now time.Time, grace time.Duration) (store.YTRecovery, error)
}

// ytFeedRunner runs one pass of every registered change-feed consumer
// (store.RunYTFeedConsumers, hk 3997).
type ytFeedRunner interface {
	RunYTFeedConsumers(ctx context.Context, limit int) ([]store.YTFeedPass, error)
}

// ytIssueIntents prepares the issue ops of tasks whose project has been
// mapped since they got their issue intent (store.PrepareMappedYTIssues,
// hk 3942).
type ytIssueIntents interface {
	PrepareMappedYTIssues(ctx context.Context, limit int) (int, error)
}

// ytBinder runs one automatic binder pass (HANDOFFKEEP_YT_BIND=on, hk 4492).
type ytBinder interface {
	BindPass(ctx context.Context, now time.Time) (ytBindReport, error)
}

// ytBindReport is what one binder pass did (design §6.6).
type ytBindReport struct {
	// Scanned is how many intents the pass examined.
	Scanned int `json:"scanned"`
	// Bound is how many tasks got a binding, and Enqueued how many missing
	// comments got an op.
	Bound    int `json:"bound"`
	Enqueued int `json:"enqueued"`
	// Refused counts the refusals by class.
	Refused map[string]int `json:"refused"`
	// Errors counts the tasks whose reads or writes failed.
	Errors int `json:"errors"`
}

// ytCommentsWorker runs one sequential Drive pass every interval. Two serve
// processes (blue-green) may both run it: the ledger's compare-and-set claim
// is the boundary, so an op is sent at most once. Its ledger is scoped to the
// tenant it was configured for (store.YTTenantLedger): it neither sends nor
// searches for an op bound to another tenant.
//
// Before each Drive pass, catchUp (HANDOFFKEEP_YT_COMMENTS_CATCHUP, on
// unless set to off; with off, only for bindings issue_create ops published,
// hk 3914) prepares the ops of comments on bound tasks that the enqueue
// missed; it does nothing outside mode on. A
// comment it skipped as ambiguous is left out of later passes of this
// process, so it is logged once and never crowds out the others; a restart
// looks at it again.
//
// Every ytBindInterval, before the recovery and the catch-up, binder (when
// set, HANDOFFKEEP_YT_BIND=on) binds the tasks whose comments appeared since
// its cursor to an issue that verifies and copies the comments the issue
// provably lacks (hk 4492). It runs before the recovery so a task it binds
// admits its prepared ops instead of having them closed as stranded.
//
// Every ytHeldRecoveryInterval, before the catch-up, recovery (when set)
// closes the ops that have been held for store.YTHeldRecoveryGrace because
// their task has no binding or another one, so they stop blocking the task's
// lane. It decides each kind by that kind's own mode, so an op of a kind whose
// mode is not on is never touched, and it closes nothing whose content is
// canonical in YouTrack.
//
// After the catch-up, requeue (when set, mode on) copies again each comment
// queued behind its task's issue_create that its claim rejected because the
// binding disagreed (the task changed project while the issue was being
// created) and whose binding every local source agrees with now: a new op at
// the tail of the task's lane, so it waits only for ops before it (hk 4569).
//
// After the requeue, feed (when set) runs each registered change-feed
// consumer once, up to ytFeedPassLimit rows each, so the ops they prepare
// go out in the same pass's Drive. With no consumer registered it does
// nothing. Then intents (when set) prepares the issue ops of this tenant's
// intents whose task's project got its mapping (nothing outside issue mode
// on).
type ytCommentsWorker struct {
	protocol *ytwrite.Protocol
	catchUp  ytCommentCatchUpper
	feed     ytFeedRunner
	intents  ytIssueIntents
	recovery ytHeldRecoverer
	// requeue copies rejected queued comments again (mode on only).
	requeue ytCommentRequeuer
	// binder is the automatic binder (HANDOFFKEEP_YT_BIND=on, hk 4492), when
	// set.
	binder ytBinder
	// tenant is the tenant the worker acts for; recovery closes only its ops
	// and the binder reads and binds only its tenant's tasks.
	tenant   string
	interval time.Duration
	// now is the clock of the recovery and binder passes (nil: the wall
	// clock).
	now func() time.Time
}

// recoverHeld runs the held-op recovery pass when ytHeldRecoveryInterval has
// passed since last, and records the time of the run in last.
func (w ytCommentsWorker) recoverHeld(ctx context.Context, last *time.Time) {
	if w.recovery == nil {
		return
	}
	now := time.Now().UTC()
	if w.now != nil {
		now = w.now().UTC()
	}
	if !last.IsZero() && now.Sub(*last) < ytHeldRecoveryInterval {
		return
	}
	*last = now
	got, err := w.recovery.RecoverHeldYTOps(ctx, w.tenant, now, store.YTHeldRecoveryGrace)
	if err != nil && ctx.Err() == nil {
		log.Printf("ytwrite.held_recovery_failed err=%q", err.Error())
	}
	if got.Rejected > 0 {
		log.Printf("ytwrite.held_recovery rejected=%d kept=%d", got.Rejected, got.Kept)
	}
}

// requeueMoved runs one requeue pass for the worker's tenant (hk 4569).
func (w ytCommentsWorker) requeueMoved(ctx context.Context) {
	if w.requeue == nil {
		return
	}
	got, err := w.requeue.RequeueYTMovedComments(ctx, w.tenant, ytCommentsRequeueLimit)
	if err != nil && ctx.Err() == nil {
		log.Printf("ytwrite.comment_requeue_failed err=%q", err.Error())
	}
	if len(got) > 0 {
		log.Printf("ytwrite.comment_requeue requeued=%d", len(got))
	}
}

// bindPass runs the automatic binder when ytBindInterval has passed since
// last, and records the time of the run in last. It runs before the held-op
// recovery so a task the pass binds admits its prepared ops instead of having
// them closed as stranded.
func (w ytCommentsWorker) bindPass(ctx context.Context, last *time.Time) {
	if w.binder == nil {
		return
	}
	now := time.Now().UTC()
	if w.now != nil {
		now = w.now().UTC()
	}
	if !last.IsZero() && now.Sub(*last) < ytBindInterval {
		return
	}
	*last = now
	if _, err := w.binder.BindPass(ctx, now); err != nil && ctx.Err() == nil {
		log.Printf("ytwrite.bind_pass_failed err=%q", err.Error())
	}
}

func (w ytCommentsWorker) Run(ctx context.Context) {
	interval := w.interval
	if interval <= 0 {
		interval = ytCommentsInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	skipped := []int64{}
	var recovered, bound time.Time
	for {
		w.bindPass(ctx, &bound)
		w.recoverHeld(ctx, &recovered)
		if w.catchUp != nil {
			got, err := w.catchUp.CatchUpYTComments(ctx, ytCommentsCatchUpLimit, skipped)
			if err != nil && ctx.Err() == nil {
				log.Printf("ytwrite.comment_catchup_failed err=%q", err.Error())
			}
			skipped = append(skipped, got.Skipped...)
		}
		w.requeueMoved(ctx)
		if w.feed != nil {
			// Each failing consumer is logged by the store (ytwrite.feed_failed).
			_, _ = w.feed.RunYTFeedConsumers(ctx, ytFeedPassLimit)
		}
		if w.intents != nil {
			if _, err := w.intents.PrepareMappedYTIssues(ctx, ytCommentsCatchUpLimit); err != nil && ctx.Err() == nil {
				log.Printf("ytwrite.issue_intents_failed err=%q", err.Error())
			}
		}
		st, err := w.protocol.Drive(ctx, ytCommentsPassLimit)
		if err != nil && ctx.Err() == nil {
			log.Printf("ytwrite.drive_failed err=%q", err.Error())
		}
		if st != (ytwrite.DriveStats{}) {
			log.Printf("ytwrite.drive recovered=%d visited=%d reconciled=%d applied=%d", st.Recovered, st.Visited, st.Reconciled, st.Applied)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// The automatic binder (hk 4492, design/2026-11-13/yt-comment-binding-lifecycle
// §6.6, §6.7). HANDOFFKEEP_YT_BIND=on makes the comment worker, every
// ytBindInterval, take the tasks whose comments appeared since its cursor and
// bind the ones whose YouTrack issue verifies, with a watermark computed from
// the issue's own listing, then copy the comments the issue provably lacks.
// It never creates an issue, never rebinds a bound task, and never undoes an
// operator unbind (I7).

// The classes an automatic binding is refused or ended with (design §6.6,
// §6.8).
const (
	ytBindNoIssue      = "no_issue"
	ytBindAmbiguous    = "ambiguous"
	ytBindHistory      = "history_ambiguous"
	ytBindHistoryLarge = "history_large"
	ytBindOtherTenant  = "other_tenant"
)

// ytBindRemote is what the binder needs from YouTrack: the comment listing
// and the issue read the enrollment rules use.
type ytBindRemote interface {
	ytwrite.Remote
	youtrack.IssueReader
}

// ytBindingBinder settles the durable intents the store's cursor pass
// recorded (store.ScanYTBindingCandidates). bucketID is the HK bucket project
// a task with no hk project verifies against.
type ytBindingBinder struct {
	st       *store.Store
	remote   ytBindRemote
	tenant   string
	bucketID string
}

func newYTBindingBinder(st *store.Store, remote ytwrite.Remote, tenant, bucketID string) (ytBindingBinder, error) {
	r, ok := remote.(ytBindRemote)
	if !ok {
		return ytBindingBinder{}, errors.New("the automatic binder needs a remote that reads issues")
	}
	return ytBindingBinder{st: st, remote: r, tenant: tenant, bucketID: bucketID}, nil
}

// BindPass reads the next page of the comment stream into intents and settles
// up to ytBindPassLimit due ones. One task's failure never stops the pass.
func (b ytBindingBinder) BindPass(ctx context.Context, now time.Time) (ytBindReport, error) {
	rep := ytBindReport{Refused: map[string]int{}}
	scan, err := b.st.ScanYTBindingCandidates(ctx, b.tenant, ytBindScanLimit, now)
	if err != nil {
		return rep, err
	}
	intents, err := b.st.YTBindingIntentsDue(ctx, b.tenant, now, ytBindPassLimit)
	if err != nil {
		return rep, err
	}
	for _, in := range intents {
		rep.Scanned++
		b.bindTask(ctx, now, in, &rep)
	}
	log.Printf("ytwrite.bind_pass scanned=%d bound=%d enqueued=%d errors=%d refused=%q queued=%d",
		rep.Scanned, rep.Bound, rep.Enqueued, rep.Errors, ytBindRefusedText(rep.Refused), scan.Tasks)
	return rep, nil
}

// ytBindRefusedText renders the refusal counts as class:count pairs, in
// class order, for one log line.
func ytBindRefusedText(refused map[string]int) string {
	classes := make([]string, 0, len(refused))
	for class := range refused {
		classes = append(classes, class)
	}
	sort.Strings(classes)
	parts := make([]string, 0, len(classes))
	for _, class := range classes {
		parts = append(parts, fmt.Sprintf("%s:%d", class, refused[class]))
	}
	return strings.Join(parts, ",")
}

// bindTask moves one intent: excluded when the operator unbound the task,
// finished when the task is bound already, or verified, published and copied
// otherwise.
func (b ytBindingBinder) bindTask(ctx context.Context, now time.Time, in store.YTBindingIntent, rep *ytBindReport) {
	// 1. An operator unbind outlives the deleted row and is never undone
	// (I7): no YouTrack call, no retry.
	excluded, err := b.st.YTBindingExcluded(ctx, in.TaskID)
	if err != nil {
		b.fail(ctx, now, in.TaskID, err, rep)
		return
	}
	if excluded {
		b.exclude(ctx, now, in.TaskID, "excluded", "the operator unbound the task", rep)
		return
	}
	// 2. A task bound by anyone already (the backfill, an applied
	// issue_create, another worker) owns its comments_from and everything at
	// or below it: the intent is done (B2 in the consult's design).
	if bound, found, err := b.st.YTTaskBinding(ctx, in.TaskID); err != nil {
		b.fail(ctx, now, in.TaskID, err, rep)
		return
	} else if found {
		b.settleBound(ctx, now, in.TaskID, bound, 0, rep)
		return
	}
	// 3. Verify the issue with the enrollment rules (design §6.8).
	candidate, err := b.st.YTCanaryCandidate(ctx, in.TaskID)
	if err != nil {
		b.fail(ctx, now, in.TaskID, err, rep)
		return
	}
	gctx, cancel := context.WithTimeout(ctx, ytCommentsGetTimeout)
	binding, err := youtrack.VerifyBinding(gctx, b.remote, b.tenant, b.bucketID, candidate)
	cancel()
	switch {
	case errors.Is(err, youtrack.ErrNoIssue):
		b.refuse(ctx, now, in.TaskID, ytBindNoIssue, err.Error(), rep)
		return
	case errors.Is(err, youtrack.ErrCanaryRefused):
		b.refuse(ctx, now, in.TaskID, ytBindAmbiguous, err.Error(), rep)
		return
	case err != nil:
		b.fail(ctx, now, in.TaskID, err, rep)
		return
	}
	binding.BoundBy = ytBindActor
	// 4. The copies come from positive evidence (I8); a history that is not
	// provably complete, or too large to copy in one transaction, refuses.
	ev, class, err := b.evidence(ctx, in.TaskID, binding.IssueID)
	if err != nil {
		b.fail(ctx, now, in.TaskID, err, rep)
		return
	}
	if class != "" {
		b.refuse(ctx, now, in.TaskID, class, "the comment history is not provably complete", rep)
		return
	}
	// 5. Publish the binding and its copies in one transaction. The
	// publication rechecks the unbind and the comments authority under its
	// locks and never adopts another publisher's binding.
	got, err := b.st.PublishYTAutoBinding(ctx, binding, candidate, ev, ytBindActor)
	switch {
	case errors.Is(err, store.ErrYTBindingExcluded):
		b.exclude(ctx, now, in.TaskID, "excluded", err.Error(), rep)
		return
	case errors.Is(err, store.ErrYTBindingExists):
		if bound, found, rerr := b.st.YTTaskBinding(ctx, in.TaskID); rerr != nil {
			b.fail(ctx, now, in.TaskID, rerr, rep)
		} else if found {
			b.settleBound(ctx, now, in.TaskID, bound, 0, rep)
		} else {
			b.fail(ctx, now, in.TaskID, err, rep)
		}
		return
	case errors.Is(err, store.ErrYTBindingConflict):
		b.refuse(ctx, now, in.TaskID, ytBindAmbiguous, err.Error(), rep)
		return
	case err != nil:
		b.fail(ctx, now, in.TaskID, err, rep)
		return
	}
	rep.Bound++
	rep.Enqueued += got.Prepared
	b.settleBound(ctx, now, in.TaskID, got.Binding, got.Prepared, rep)
}

// settleBound drops the task's intent. A binding exists, and its publisher
// owns the copies at or below its watermark; the ones above it are the
// issue-only catch-up's. A binding on another tenant excludes the task here.
// A crash between the publication's commit and this call is harmless: the
// next pass takes step 2 and drops the intent.
func (b ytBindingBinder) settleBound(ctx context.Context, now time.Time, taskID int64, bound store.YTTaskBinding, prepared int, rep *ytBindReport) {
	if bound.Tenant != b.tenant {
		b.exclude(ctx, now, taskID, ytBindOtherTenant, "bound on tenant "+bound.Tenant, rep)
		return
	}
	if err := b.st.DropYTBindingIntent(ctx, taskID); err != nil {
		log.Printf("ytwrite.bind_settle_failed task_id=%d err=%q", taskID, err.Error())
	}
	log.Printf("ytwrite.bind_task task_id=%d class=bound issue_id=%q origin=%s prepared=%d", taskID, bound.IssueID, bound.Origin, prepared)
}

// evidence lists the issue's comments once and classifies the task's
// comments against them with the audit's matcher (design §6.7). The
// publication rechecks the authority it read and copies the Missing set in
// its own transaction, so nothing here is owed after the publication commits.
// A history that is not provably complete, or larger than one publication
// copies, is a class the intent refuses with.
func (b ytBindingBinder) evidence(ctx context.Context, taskID int64, issueID string) (store.YTAutoBindEvidence, string, error) {
	authority, err := b.st.YTAuthorityOf(ctx, store.YTAuthorityComments)
	if err != nil {
		return store.YTAutoBindEvidence{}, "", err
	}
	ev := store.YTAutoBindEvidence{Authority: authority}
	if authority == store.YTAuthorityYouTrack {
		// YouTrack already holds the comments; the publication applies the
		// in-transaction max-id watermark and copies nothing.
		return ev, "", nil
	}
	hk, err := b.st.YTAuditTaskComments(ctx, taskID, issueID)
	if err != nil {
		return ev, "", err
	}
	lctx, cancel := context.WithTimeout(ctx, ytCommentsGetTimeout)
	listed, err := b.remote.ListComments(lctx, issueID, ytAuditPageSize)
	cancel()
	if err != nil {
		return ev, "", err
	}
	// A YouTrack comment the activity pull copied into hk is not the
	// issue's evidence. The legacy push's links are, by its own rules.
	yt := listed[:0:0]
	for _, c := range listed {
		if !hk.PulledYTIDs[c.ID] {
			yt = append(yt, c)
		}
	}
	for i, c := range hk.Comments {
		if link := hk.Links[c.ID]; youtrack.CommentLinkIsID(link) {
			hk.Comments[i].LinkedYTIDs = append(hk.Comments[i].LinkedYTIDs, link)
		}
	}
	got := ytwrite.AuditComments(hk.Comments, yt, "", youtrack.LegacyCommentID)
	if len(got.AmbiguousCopy) > 0 || len(got.Duplicate) > 0 {
		return ev, ytBindHistory, nil
	}
	missing := make([]int64, 0, len(got.Missing))
	for _, x := range got.Present {
		if x.CommentID > ev.CommentsFrom {
			ev.CommentsFrom = x.CommentID
		}
	}
	for _, x := range got.Missing {
		// A comment the adoption linked was posted in a form hk cannot
		// recognise; a second copy may be the result (R2).
		if youtrack.CommentLinkIsAdopted(hk.Links[x.CommentID]) {
			return ev, ytBindHistory, nil
		}
		missing = append(missing, x.CommentID)
	}
	if len(missing) > ytBindCopyMax {
		return ev, ytBindHistoryLarge, nil
	}
	ev.Missing = missing
	return ev, "", nil
}

// refuse settles an intent as refused with its class and the backoff, and
// counts it.
func (b ytBindingBinder) refuse(ctx context.Context, now time.Time, taskID int64, class, detail string, rep *ytBindReport) {
	rep.Refused[class]++
	if err := b.st.RetryYTBindingIntent(ctx, taskID, store.YTBindingIntentRefused, class, detail, now); err != nil {
		log.Printf("ytwrite.bind_settle_failed task_id=%d err=%q", taskID, err.Error())
	}
	log.Printf("ytwrite.bind_task task_id=%d class=%s detail=%q", taskID, class, detail)
}

// exclude settles an intent as terminal: no automatic binding will try the
// task again.
func (b ytBindingBinder) exclude(ctx context.Context, now time.Time, taskID int64, class, detail string, rep *ytBindReport) {
	rep.Refused[class]++
	if err := b.st.ExcludeYTBindingIntent(ctx, taskID, class, now); err != nil {
		log.Printf("ytwrite.bind_settle_failed task_id=%d err=%q", taskID, err.Error())
	}
	log.Printf("ytwrite.bind_task task_id=%d class=%s detail=%q", taskID, class, detail)
}

// fail settles an intent as an error with the backoff: a read or a write
// failed, and the next pass tries again.
func (b ytBindingBinder) fail(ctx context.Context, now time.Time, taskID int64, cause error, rep *ytBindReport) {
	rep.Errors++
	if err := b.st.RetryYTBindingIntent(ctx, taskID, store.YTBindingIntentError, "", cause.Error(), now); err != nil {
		log.Printf("ytwrite.bind_settle_failed task_id=%d err=%q", taskID, err.Error())
	}
	log.Printf("ytwrite.bind_task task_id=%d class=error detail=%q", taskID, cause.Error())
}

// ytOpsAlertWorker scans the durable ledgers independently of dispatch mode,
// so pending ops and deploy lag still alert if comment sending is turned off.
type ytOpsAlertWorker struct {
	store    *store.Store
	interval time.Duration
}

// opsAlertPasses is the set of outbox passes the alert worker runs each tick.
type opsAlertPasses interface {
	EnqueueYTWriteOpAlerts(ctx context.Context, now time.Time) (int, error)
	EnqueueDeployLagNotices(ctx context.Context, now time.Time) (int, error)
}

// runOpsAlertPasses runs every operator-desk notice pass once: pending
// YouTrack write ops and deploy lag (hk 3291).
func runOpsAlertPasses(ctx context.Context, p opsAlertPasses, now time.Time) {
	if _, err := p.EnqueueYTWriteOpAlerts(ctx, now); err != nil && ctx.Err() == nil {
		log.Printf("ytwrite.alerts_failed err=%q", err.Error())
	}
	if _, err := p.EnqueueDeployLagNotices(ctx, now); err != nil && ctx.Err() == nil {
		log.Printf("deploy_lag.enqueue_failed err=%q", err.Error())
	}
}

func (w ytOpsAlertWorker) Run(ctx context.Context) {
	interval := w.interval
	if interval <= 0 {
		interval = ytCommentsInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		runOpsAlertPasses(ctx, w.store, time.Now().UTC())
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

const youtrackOpsUsage = "usage: youtrack ops [--state pending|all|prepared|dispatched|applied|rejected|remote_unknown] [--kind K] [--task N] [--after-id N] [--limit N] | youtrack ops resolve <op id> --applied <youtrack id> [--adopted] | --not-applied --by <actor> --note <text>"

// youtrackOpsCmd is `youtrack ops` (also `yt ops`): the write-protocol ops
// through GET /v1/yt/ops. remote_unknown shows status pending, never done.
func youtrackOpsCmd(args []string, out io.Writer) error {
	if len(args) > 0 && args[0] == "resolve" {
		return youtrackOpsResolveCmd(args[1:], out)
	}
	fs := flag.NewFlagSet("youtrack ops", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	state := fs.String("state", "pending", "pending (default), all, or one lifecycle state")
	kind := fs.String("kind", "", "only ops of this kind (comment_append, issue_create, issue_fields, ...)")
	task := fs.Int64("task", 0, "only ops of this task")
	after := fs.Int64("after-id", 0, "ops with id greater than this")
	limit := fs.Int("limit", 100, "maximum results")
	ordered, err := splitArgs(args, "state", "kind", "task", "after-id", "limit", "url", "token")
	if err != nil {
		return err
	}
	if err := fs.Parse(ordered); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New(youtrackOpsUsage)
	}
	if s := *state; s != "pending" && s != "all" && !ytwrite.State(s).Valid() {
		return fmt.Errorf("unknown --state %q; %s", s, youtrackOpsUsage)
	}
	if *kind != "" && !slices.Contains(ytwrite.RegisteredKinds(), *kind) {
		return fmt.Errorf("unknown --kind %q; %s", *kind, youtrackOpsUsage)
	}
	if *task < 0 || *after < 0 || *limit < 1 || *limit > store.YTOpListMax {
		return errors.New(youtrackOpsUsage)
	}
	if err := mustClient(c); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ops, next, err := c.ListYTOps(ctx, *state, *kind, *task, *after, *limit)
	if err != nil {
		return err
	}
	out2 := map[string]any{"ops": ops, "next_after_id": nil}
	if next > 0 {
		out2["next_after_id"] = next
	}
	return printJSON(out, out2)
}

func youtrackOpsResolveCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("youtrack ops resolve", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	appliedID := fs.String("applied", "", "YouTrack comment id when the op was applied")
	notApplied := fs.Bool("not-applied", false, "record that the comment was not applied")
	adopted := fs.Bool("adopted", false, "with --applied on an issue_create op: the issue existed before the op (its history is not copied again)")
	by := fs.String("by", "", "operator who checked YouTrack")
	note := fs.String("note", "", "operator's resolution note")
	c := remoteClient(fs)
	ordered, err := splitArgs(args, "applied", "by", "note", "url", "token")
	if err != nil {
		return err
	}
	if err := fs.Parse(ordered); err != nil {
		return err
	}
	if fs.NArg() != 1 || (*appliedID != "") == *notApplied || (*adopted && *notApplied) {
		return errors.New(youtrackOpsUsage)
	}
	if *notApplied && (strings.TrimSpace(*by) == "" || strings.TrimSpace(*note) == "") {
		return errors.New(youtrackOpsUsage)
	}
	id, err := strconv.ParseInt(fs.Arg(0), 10, 64)
	if err != nil || id < 1 {
		return errors.New("op id must be positive")
	}
	if err := mustClient(c); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// An issue_create op needs the provenance stated (created unless
	// --adopted); every other op keeps the request shape a server that
	// predates the field accepts, so the op's kind is read first (GET
	// /v1/yt/ops, which every release serves).
	var provenance *bool
	if !*notApplied {
		issue, err := ytOpIsIssueCreate(ctx, *c, id)
		if err != nil {
			return err
		}
		if issue || *adopted {
			provenance = adopted
		}
	}
	actor := *by
	if actor == "" {
		actor = "operator:cli"
	}
	result, err := c.ResolveYTOp(ctx, id, ytwrite.Resolution{Applied: !*notApplied, YTID: *appliedID, Adopted: provenance, By: actor, Note: *note})
	if err != nil {
		return err
	}
	return printJSON(out, map[string]any{"op_id": result.OpID, "status": result.Status, "state": result.State, "reason": result.Reason, "yt_id": result.YTID})
}

// ytOpIsIssueCreate reads whether op id is an issue_create op. An op the
// listing does not return is not one; the server answers the resolution.
func ytOpIsIssueCreate(ctx context.Context, c remote.Client, id int64) (bool, error) {
	ops, _, err := c.ListYTOps(ctx, "all", "", 0, id-1, 1)
	if err != nil {
		return false, err
	}
	return len(ops) == 1 && ops[0].ID == id && ops[0].Kind == ytwrite.KindIssueCreate, nil
}

const youtrackCanaryUsage = "usage: youtrack canary add <task> [--by NAME] | remove <task> | list"

// youtrackCanaryCmd keeps the comment canary set (slice b1a) directly in PG,
// like `youtrack projects`. add verifies the task's imported issue in
// YouTrack first and stores the binding; it writes no op row.
func youtrackCanaryCmd(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(youtrackCanaryUsage)
	}
	fs := flag.NewFlagSet("youtrack canary", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	by := fs.String("by", "operator:cli", "who enrolled the task")
	ordered, err := splitArgs(args[1:], "by")
	if err != nil {
		return err
	}
	if err := fs.Parse(ordered); err != nil {
		return err
	}
	var taskID int64
	switch args[0] {
	case "add", "remove":
		if fs.NArg() != 1 {
			return errors.New(youtrackCanaryUsage)
		}
		if taskID, err = strconv.ParseInt(fs.Arg(0), 10, 64); err != nil || taskID < 1 {
			return errors.New("task id must be positive")
		}
	case "list":
		if fs.NArg() != 0 {
			return errors.New(youtrackCanaryUsage)
		}
	default:
		return errors.New(youtrackCanaryUsage)
	}
	dbURL := strings.TrimSpace(os.Getenv("HANDOFFKEEP_DB_URL"))
	if dbURL == "" {
		return exitCodeError{code: youtrackExitConfig, err: errors.New("HANDOFFKEEP_DB_URL is required")}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, err := store.Open(ctx, dbURL)
	if err != nil {
		return err
	}
	defer st.Close()
	switch args[0] {
	case "list":
		set, err := st.ListYTCommentCanary(ctx)
		if err != nil {
			return err
		}
		return printJSON(out, map[string]any{"canary": set})
	case "remove":
		removed, err := st.RemoveYTCommentCanary(ctx, taskID)
		if err != nil {
			return err
		}
		return printJSON(out, map[string]any{"task_id": taskID, "removed": removed})
	}
	baseURL := strings.TrimSpace(os.Getenv("YOUTRACK_URL"))
	token := strings.TrimSpace(os.Getenv("YOUTRACK_BOT_API_KEY"))
	if baseURL == "" || token == "" {
		return exitCodeError{code: youtrackExitConfig, err: errors.New("YOUTRACK_URL and YOUTRACK_BOT_API_KEY are required to verify the issue")}
	}
	client, err := youtrack.New(youtrack.Config{BaseURL: baseURL, Token: token, UserAgent: "handoffkeep-ytwrite"})
	if err != nil {
		return exitCodeError{code: youtrackExitConfig, err: err}
	}
	remote := youtrack.NewWriteRemote(client)
	candidate, err := st.YTCanaryCandidate(ctx, taskID)
	if err != nil {
		return err
	}
	binding, err := youtrack.VerifyCanary(ctx, remote, remote.Tenant(), candidate)
	if err != nil {
		return err
	}
	binding.AddedBy = *by
	got, err := st.AddYTCommentCanary(ctx, binding, candidate)
	if err != nil {
		return err
	}
	return printJSON(out, got)
}

// Comment mode on is turned on for tasks with a verified binding
// (yt_task_bindings). `youtrack comments-preflight` reports which tasks would
// be covered and writes nothing; `youtrack comments-backfill` makes the same
// classification and binds the tasks that verify. Both open the database
// directly, like `youtrack canary`, and verify against the tenant the comment
// worker uses (the host of YOUTRACK_URL). docs/youtrack-write-protocol.md.

// The classes of a candidate task.
const (
	// ytClassBound: bound on this tenant and every local source agrees.
	ytClassBound = "bound"
	// ytClassBoundAmbiguous: bound, but a local source disagrees with the
	// binding (or it is on another tenant): mode on skips its comments.
	ytClassBoundAmbiguous = "bound_ambiguous"
	// ytClassVerifies: not bound, and its issue verifies in YouTrack.
	ytClassVerifies = "verifies"
	// ytClassNoIssue: no project, an unmapped project, or no such issue.
	ytClassNoIssue = "no_issue"
	// ytClassAmbiguous: a refusal caused by a disagreement, or a binding
	// the publication refused.
	ytClassAmbiguous = "ambiguous"
	// ytClassError: a read failed; run again.
	ytClassError = "error"
)

const (
	ytCommentsDefaultDays = 14
	// ytCommentsSample bounds the task ids listed per class.
	ytCommentsSample = 10
	// ytCommentsGetTimeout bounds one YouTrack read; ytCommentsTaskTimeout
	// one task's classification (its reads and that GET) and its binding.
	// There is no budget for the whole run, so a long run never stops
	// half-way.
	ytCommentsGetTimeout  = 15 * time.Second
	ytCommentsTaskTimeout = 60 * time.Second
)

// ytCommentsClass is one class of one project: how many tasks, and the first
// ytCommentsSample ids in task id order.
type ytCommentsClass struct {
	Count   int     `json:"count"`
	TaskIDs []int64 `json:"task_ids"`
}

func (c *ytCommentsClass) add(taskID int64) {
	c.Count++
	if len(c.TaskIDs) < ytCommentsSample {
		c.TaskIDs = append(c.TaskIDs, taskID)
	}
}

type ytCommentsProject struct {
	Project        string          `json:"project"`
	Bound          ytCommentsClass `json:"bound"`
	BoundAmbiguous ytCommentsClass `json:"bound_ambiguous"`
	Verifies       ytCommentsClass `json:"verifies"`
	NoIssue        ytCommentsClass `json:"no_issue"`
	Ambiguous      ytCommentsClass `json:"ambiguous"`
	Error          ytCommentsClass `json:"error"`
}

func (p *ytCommentsProject) class(name string) *ytCommentsClass {
	switch name {
	case ytClassBound:
		return &p.Bound
	case ytClassBoundAmbiguous:
		return &p.BoundAmbiguous
	case ytClassVerifies:
		return &p.Verifies
	case ytClassNoIssue:
		return &p.NoIssue
	case ytClassAmbiguous:
		return &p.Ambiguous
	default:
		return &p.Error
	}
}

// ytCommentsReport is the output of both commands. BoundNow is set by the
// backfill only.
type ytCommentsReport struct {
	Tenant             string               `json:"tenant"`
	Days               int                  `json:"days"`
	All                bool                 `json:"all,omitempty"`
	Project            string               `json:"project,omitempty"`
	Task               int64                `json:"task,omitempty"`
	CommentsTotal      int                  `json:"comments_total"`
	MaxCommentsPerHour int                  `json:"max_comments_per_hour"`
	Projects           []*ytCommentsProject `json:"projects"`
	Uncovered          struct {
		Count int     `json:"count"`
		OpIDs []int64 `json:"op_ids"`
	} `json:"unresolved_ops_uncovered"`
	BoundNow *int `json:"bound_now,omitempty"`
	errors   int
}

// ytClassified is one task's class, with the binding and the candidate it
// was verified against when the class is verifies.
type ytClassified struct {
	class    string
	detail   string
	binding  store.YTTaskBinding
	verified store.YTCanaryCandidate
}

// classifyYTCommentTask classifies one task. A bound task is checked against
// the local sources only (no YouTrack call); an unbound one is verified in
// YouTrack (youtrack.VerifyBinding) and its would-be binding checked against
// the same local sources.
func classifyYTCommentTask(ctx context.Context, st *store.Store, issues youtrack.IssueReader, tenant, bucketID string, taskID int64) ytClassified {
	failed := func(err error) ytClassified { return ytClassified{class: ytClassError, detail: err.Error()} }
	b, found, err := st.YTTaskBinding(ctx, taskID)
	if err != nil {
		return failed(err)
	}
	if found {
		if b.Tenant != tenant {
			return ytClassified{class: ytClassBoundAmbiguous, detail: "bound on tenant " + b.Tenant}
		}
		source, err := st.YTBindingDisagreement(ctx, b)
		if err != nil {
			return failed(err)
		}
		if source != "" {
			return ytClassified{class: ytClassBoundAmbiguous, detail: source + " disagrees with the binding"}
		}
		return ytClassified{class: ytClassBound}
	}
	verified, err := st.YTCanaryCandidate(ctx, taskID)
	if err != nil {
		return failed(err)
	}
	gctx, cancel := context.WithTimeout(ctx, ytCommentsGetTimeout)
	b, err = youtrack.VerifyBinding(gctx, issues, tenant, bucketID, verified)
	cancel()
	switch {
	case errors.Is(err, youtrack.ErrNoIssue):
		return ytClassified{class: ytClassNoIssue, detail: err.Error()}
	case errors.Is(err, youtrack.ErrCanaryRefused):
		return ytClassified{class: ytClassAmbiguous, detail: err.Error()}
	case err != nil:
		return failed(err)
	}
	source, err := st.YTBindingDisagreement(ctx, b)
	if err != nil {
		return failed(err)
	}
	if source != "" {
		return ytClassified{class: ytClassAmbiguous, detail: source + " disagrees with issue " + b.IssueID}
	}
	return ytClassified{class: ytClassVerifies, binding: b, verified: verified}
}

// ytCommentsOptions selects what one preflight or backfill run covers.
type ytCommentsOptions struct {
	// Project keeps one hk project's tasks ("" for all).
	Project string
	// Days is the comment window: candidates (unless All) and the volume.
	Days int
	// All makes every task a candidate (bind-all, hk 3897).
	All bool
	// Task, when not 0, makes that one task the only candidate (a rebind
	// after `youtrack comments-unbind`); Project and All are then unused.
	Task int64
	// BucketID is the YouTrack project a task with no hk project verifies
	// against (youtrack.ResolveBucketProject).
	BucketID string
	// BindBy, when not "" (the backfill), publishes each verified binding.
	BindBy string
}

// ytCommentsRun classifies every candidate and, when o.BindBy is not "" (the
// backfill), publishes each verified binding; a refused publication counts
// as ambiguous and the run goes on. It never stops on one task.
func ytCommentsRun(ctx context.Context, st *store.Store, issues youtrack.IssueReader, tenant string, o ytCommentsOptions) (ytCommentsReport, error) {
	project, days, bindBy := o.Project, o.Days, o.BindBy
	r := ytCommentsReport{Tenant: tenant, Days: days, All: o.All, Project: project, Task: o.Task, Projects: []*ytCommentsProject{}}
	r.Uncovered.OpIDs = []int64{}
	since := time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour)
	var err error
	if r.CommentsTotal, r.MaxCommentsPerHour, err = st.YTCommentVolume(ctx, since, project); err != nil {
		return r, err
	}
	var candidates []store.YTCommentCandidate
	if o.Task > 0 {
		c, found, err := st.YTCommentCandidateOf(ctx, o.Task)
		if err != nil {
			return r, err
		}
		if !found {
			return r, fmt.Errorf("task %d does not exist", o.Task)
		}
		candidates = []store.YTCommentCandidate{c}
	} else if candidates, err = st.YTCommentCandidates(ctx, since, project, o.All); err != nil {
		return r, err
	}
	byProject := map[string]*ytCommentsProject{}
	candidate := map[int64]bool{}
	boundNow := 0
	for _, c := range candidates {
		candidate[c.TaskID] = true
		tctx, cancel := context.WithTimeout(ctx, ytCommentsTaskTimeout)
		got := classifyYTCommentTask(tctx, st, issues, tenant, o.BucketID, c.TaskID)
		if bindBy != "" && got.class == ytClassVerifies {
			b := got.binding
			b.BoundBy = bindBy
			_, err := st.AddYTTaskBinding(tctx, b, got.verified)
			switch {
			case err == nil:
				boundNow++
			case errors.Is(err, store.ErrYTBindingConflict):
				got = ytClassified{class: ytClassAmbiguous, detail: err.Error()}
			default:
				got = ytClassified{class: ytClassError, detail: err.Error()}
			}
		}
		cancel()
		switch got.class {
		case ytClassError:
			r.errors++
			log.Printf("youtrack.comments_classify_failed task_id=%d err=%q", c.TaskID, got.detail)
		case ytClassBoundAmbiguous, ytClassNoIssue, ytClassAmbiguous:
			log.Printf("youtrack.comments_classified task_id=%d class=%s detail=%q", c.TaskID, got.class, got.detail)
		}
		p := byProject[c.Project]
		if p == nil {
			p = &ytCommentsProject{Project: c.Project}
			for _, name := range []string{ytClassBound, ytClassBoundAmbiguous, ytClassVerifies, ytClassNoIssue, ytClassAmbiguous, ytClassError} {
				p.class(name).TaskIDs = []int64{}
			}
			byProject[c.Project] = p
			r.Projects = append(r.Projects, p)
		}
		p.class(got.class).add(c.TaskID)
	}
	sort.Slice(r.Projects, func(i, j int) bool { return r.Projects[i].Project < r.Projects[j].Project })
	if bindBy != "" {
		r.BoundNow = &boundNow
	}
	// What mode on would hold: unresolved comment ops of candidate tasks
	// with no binding on this tenant that matches the op and agrees with
	// every local source. Read after the bindings, so a backfill reports
	// what is left.
	ops, err := st.YTUnresolvedCommentOps(ctx)
	if err != nil {
		return r, err
	}
	covered := map[int64]store.YTTaskBinding{}
	for _, op := range ops {
		if !candidate[op.TaskID] {
			continue
		}
		b, seen := covered[op.TaskID]
		if !seen {
			got, found, err := st.YTTaskBinding(ctx, op.TaskID)
			if err != nil {
				return r, err
			}
			if found && got.Tenant == tenant {
				source, err := st.YTBindingDisagreement(ctx, got)
				if err != nil {
					return r, err
				}
				if source == "" {
					b = got
				}
			}
			covered[op.TaskID] = b
		}
		if op.Tenant != tenant || b.Tenant != op.Tenant || b.IssueID != op.IssueID {
			r.Uncovered.Count++
			if len(r.Uncovered.OpIDs) < ytCommentsSample {
				r.Uncovered.OpIDs = append(r.Uncovered.OpIDs, op.OpID)
			}
		}
	}
	return r, nil
}

const (
	youtrackCommentsPreflightUsage = "usage: youtrack comments-preflight [--project P] [--days N] [--all] [--task N]"
	youtrackCommentsBackfillUsage  = "usage: youtrack comments-backfill [--project P] [--days N] [--all] [--task N] [--by NAME]"
)

// ytBindingMigrations are the registered migrations a binding read needs:
// ytBindingColumns selects comments_from, origin and op_id. The read-only
// commands check them all and refuse a schema without one, because they
// never migrate.
var ytBindingMigrations = []string{store.YTTaskBindingsMigration, store.YTBindingCommentsFromMigration, store.YTTaskBindingOriginMigration}

// youtrackCommentsCmd is `youtrack comments-preflight` (backfill false):
// read-only, it reads YouTrack issues but writes nothing, not even a
// migration (store.OpenReadOnly); and `youtrack comments-backfill` (backfill
// true), which also publishes each verified binding. The report goes to stdout; a task whose reads failed (class error)
// makes the command exit 1 after the report.
func youtrackCommentsCmd(args []string, out io.Writer, backfill bool) error {
	name, usage := "comments-preflight", youtrackCommentsPreflightUsage
	if backfill {
		name, usage = "comments-backfill", youtrackCommentsBackfillUsage
	}
	fs := flag.NewFlagSet("youtrack "+name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	project := fs.String("project", "", "only tasks of this hk project")
	days := fs.Int("days", ytCommentsDefaultDays, "tasks with a comment in the last N days")
	all := fs.Bool("all", false, "every task, not only those with a recent comment")
	task := fs.Int64("task", 0, "only this task (not with --project or --all)")
	valueFlags := []string{"project", "days", "task"}
	var by *string
	if backfill {
		by = fs.String("by", "operator:cli", "who bound the tasks")
		valueFlags = append(valueFlags, "by")
	}
	ordered, err := splitArgs(args, valueFlags...)
	if err != nil {
		return err
	}
	if err := fs.Parse(ordered); err != nil {
		return fmt.Errorf("%v; %s", err, usage)
	}
	// --task 0 is a supplied flag, not an absent one: it must not fall back
	// to the multi-task run.
	taskSet := false
	fs.Visit(func(f *flag.Flag) { taskSet = taskSet || f.Name == "task" })
	if fs.NArg() != 0 || *days < 1 || (taskSet && (*task < 1 || *project != "" || *all)) {
		return errors.New(usage)
	}
	bindBy := ""
	if backfill {
		if bindBy = strings.TrimSpace(*by); bindBy == "" {
			return errors.New(usage)
		}
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
	// The preflight never writes: it opens without migrating, on a pool
	// whose every transaction is READ ONLY, and refuses a schema from before
	// this release. The backfill is a write command and migrates as the
	// server does; yt_task_bindings is its release's own migration.
	openStore := store.OpenReadOnly
	if backfill {
		openStore = store.Open
	}
	st, err := openStore(openCtx, dbURL)
	if err != nil {
		return err
	}
	defer st.Close()
	if !backfill {
		for _, id := range ytBindingMigrations {
			applied, err := st.MigrationApplied(openCtx, id)
			if err != nil {
				return err
			}
			if !applied {
				return exitCodeError{code: youtrackExitConfig, err: fmt.Errorf("the database lacks migration %s: deploy the release that adds it first; the preflight never migrates", id)}
			}
		}
	}
	// A task with no hk project verifies against the import's bucket. Its id
	// is read once, before any task: no bucket, or two, refuses the run.
	bucketCtx, cancelBucket := context.WithTimeout(context.Background(), ytCommentsGetTimeout)
	bucketID, err := youtrack.ResolveBucketProject(bucketCtx, client)
	cancelBucket()
	if errors.Is(err, youtrack.ErrBucketProject) {
		return exitCodeError{code: youtrackExitConfig, err: err}
	}
	if err != nil {
		return exitCodeError{code: youtrackExitFailed, err: fmt.Errorf("list YouTrack projects: %w", err)}
	}
	report, err := ytCommentsRun(context.Background(), st, remote, remote.Tenant(), ytCommentsOptions{Project: *project, Days: *days, All: *all, Task: *task, BucketID: bucketID, BindBy: bindBy})
	if err != nil {
		return err
	}
	if err := printJSON(out, report); err != nil {
		return err
	}
	if report.errors > 0 {
		return exitCodeError{code: youtrackExitFailed, err: fmt.Errorf("%d tasks could not be classified (class error); run again", report.errors)}
	}
	return nil
}

const youtrackCommentsUnbindUsage = "usage: youtrack comments-unbind <task> [--by NAME] [--reason TEXT] [--force]"

// youtrackCommentsUnbindCmd is `youtrack comments-unbind` (hk 4490): it removes
// a task's binding, rejects the prepared ops the binding admitted and records
// the unbind (store.UnbindYTTaskBinding). It refuses while an op of the task
// may have reached the issue, and, with comments canonical in YouTrack, while a
// comment op is prepared unless --force. A task with no binding answers
// removed false and writes nothing. It opens the database directly, like
// `youtrack canary`, and calls no YouTrack. `youtrack comments-backfill --task
// N` binds the task again once every other record hk keeps for it (mirror
// link, canary row) agrees with the issue it verifies; one that does not is
// named in the refusal, and unbind does not touch it. The task's map row, the
// anchor its first op wrote, is released with the binding when it names the
// binding's issue (anchor_released), so a different issue can bind (hk 4536).
func youtrackCommentsUnbindCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("youtrack comments-unbind", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	by := fs.String("by", "operator:cli", "who unbinds the task")
	reason := fs.String("reason", "", "why the task is unbound")
	force := fs.Bool("force", false, "reject prepared comment ops even when comments are canonical in YouTrack")
	ordered, err := splitArgs(args, "by", "reason")
	if err != nil {
		return err
	}
	if err := fs.Parse(ordered); err != nil {
		return fmt.Errorf("%v; %s", err, youtrackCommentsUnbindUsage)
	}
	if fs.NArg() != 1 || strings.TrimSpace(*by) == "" {
		return errors.New(youtrackCommentsUnbindUsage)
	}
	taskID, err := strconv.ParseInt(fs.Arg(0), 10, 64)
	if err != nil || taskID < 1 {
		return errors.New("task id must be positive")
	}
	dbURL := strings.TrimSpace(os.Getenv("HANDOFFKEEP_DB_URL"))
	if dbURL == "" {
		return exitCodeError{code: youtrackExitConfig, err: errors.New("HANDOFFKEEP_DB_URL is required")}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, err := store.Open(ctx, dbURL)
	if err != nil {
		return err
	}
	defer st.Close()
	res, err := st.UnbindYTTaskBinding(ctx, taskID, store.YTUnbindOptions{By: strings.TrimSpace(*by), Reason: strings.TrimSpace(*reason), Force: *force})
	if err != nil {
		return err
	}
	return printJSON(out, res)
}

const youtrackCommentsDrainUsage = "usage: youtrack comments-drain [--tenant T] [--wait 10m] [--reject-held] [--task N]"

type ytCommentDrainReport struct {
	Tenant   string            `json:"tenant"`
	Task     int64             `json:"task,omitempty"`
	Modes    map[string]string `json:"modes"`
	Counts   map[string]int    `json:"counts"`
	Rejected []int64           `json:"rejected,omitempty"`
}

// youtrackCommentsDrainCmd monitors the comment ops the serve worker can
// send. It never dispatches. --reject-held reuses the recovery path so every
// rejection is serialized with the lane and authority switches.
func youtrackCommentsDrainCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("youtrack comments-drain", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	tenantFlag := fs.String("tenant", "", "YouTrack tenant host")
	wait := fs.Duration("wait", 0, "wait for ready ops to drain")
	rejectHeld := fs.Bool("reject-held", false, "close held ops that are safe to reject")
	taskID := fs.Int64("task", 0, "only classify one task")
	ordered, err := splitArgs(args, "tenant", "wait", "task")
	if err != nil {
		return err
	}
	if err := fs.Parse(ordered); err != nil || fs.NArg() != 0 || *wait < 0 || *taskID < 0 {
		return errors.New(youtrackCommentsDrainUsage)
	}
	taskSpecified := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "task" {
			taskSpecified = true
		}
	})
	if taskSpecified && *taskID < 1 {
		return errors.New(youtrackCommentsDrainUsage)
	}
	mode, err := ytCommentsModeFromEnv()
	if err != nil {
		return exitCodeError{code: youtrackExitConfig, err: err}
	}
	if *rejectHeld && mode != store.YTCommentsOn {
		return exitCodeError{code: youtrackExitConfig, err: errors.New("--reject-held requires HANDOFFKEEP_YT_COMMENTS_MODE=on")}
	}
	fields, err := ytFieldsModeFromEnv()
	if err != nil {
		return exitCodeError{code: youtrackExitConfig, err: err}
	}
	moves, err := ytMovesModeFromEnv()
	if err != nil {
		return exitCodeError{code: youtrackExitConfig, err: err}
	}
	if moves == store.YTMovesOn && fields != store.YTFieldsOn {
		return exitCodeError{code: youtrackExitConfig, err: errors.New("HANDOFFKEEP_YT_MOVES_MODE=on requires HANDOFFKEEP_YT_FIELDS_MODE=on")}
	}
	tenant := strings.TrimSpace(*tenantFlag)
	if tenant == "" {
		u, parseErr := url.Parse(strings.TrimSpace(os.Getenv("YOUTRACK_URL")))
		if parseErr == nil && u != nil && u.Host != "" {
			tenant = u.Host
		}
	}
	if tenant == "" {
		return exitCodeError{code: youtrackExitConfig, err: errors.New("--tenant or YOUTRACK_URL is required")}
	}
	dbURL := strings.TrimSpace(os.Getenv("HANDOFFKEEP_DB_URL"))
	if dbURL == "" {
		return exitCodeError{code: youtrackExitConfig, err: errors.New("HANDOFFKEEP_DB_URL is required")}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second+*wait)
	defer cancel()
	var st *store.Store
	if *rejectHeld {
		st, err = store.Open(ctx, dbURL)
	} else {
		st, err = store.OpenReadOnly(ctx, dbURL)
	}
	if err != nil {
		return err
	}
	defer st.Close()
	st.SetYTCommentsMode(mode)
	st.SetYTFieldsMode(fields, tenant)
	st.SetYTMovesMode(moves)

	deadline := time.Now().Add(*wait)
	var report ytCommentDrainReport
	for {
		report, err = ytCommentDrainSnapshot(ctx, st, tenant, *taskID, mode, fields, moves)
		if err != nil {
			return err
		}
		if report.Counts["remote_unknown"] > 0 {
			if err := printJSON(out, report); err != nil {
				return err
			}
			return exitCodeError{code: youtrackExitRemoteUnknown, err: errors.New("remote_unknown comment ops require `youtrack ops resolve`")}
		}
		if report.Counts["ready"] == 0 || time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return exitCodeError{code: youtrackExitPending, err: ctx.Err()}
		case <-time.After(time.Second):
		}
	}
	if *rejectHeld {
		var got store.YTRecovery
		if *taskID > 0 {
			got, err = st.RecoverHeldYTOpsForTask(ctx, tenant, *taskID, time.Now().UTC(), 0)
		} else {
			got, err = st.RecoverHeldYTOps(ctx, tenant, time.Now().UTC(), 0)
		}
		if err != nil {
			return err
		}
		report.Rejected = got.OpIDs
		report, err = ytCommentDrainSnapshot(ctx, st, tenant, *taskID, mode, fields, moves)
		if err != nil {
			return err
		}
		report.Rejected = got.OpIDs
		if report.Counts["remote_unknown"] > 0 {
			if err := printJSON(out, report); err != nil {
				return err
			}
			return exitCodeError{code: youtrackExitRemoteUnknown, err: errors.New("remote_unknown comment ops require `youtrack ops resolve`")}
		}
	}
	if err := printJSON(out, report); err != nil {
		return err
	}
	if report.Counts["ready"] > 0 {
		return exitCodeError{code: youtrackExitPending, err: errors.New("comment ops are still ready; serve has not drained them")}
	}
	return nil
}

const (
	youtrackExitPending       = 3
	youtrackExitRemoteUnknown = 4
)

func ytCommentDrainSnapshot(ctx context.Context, st *store.Store, tenant string, taskID int64, comments store.YTCommentsMode, fields store.YTFieldsMode, moves store.YTMovesMode) (ytCommentDrainReport, error) {
	report := ytCommentDrainReport{Tenant: tenant, Task: taskID, Modes: map[string]string{"comments": string(comments), "fields": string(fields), "moves": string(moves)}, Counts: map[string]int{
		"ready": 0, "held_binding": 0, "held_binding_changed": 0, "held_other_tenant": 0,
		"held_authority_youtrack": 0, "held_ambiguous": 0, "held_mode": 0, "remote_unknown": 0,
	}}
	commentAuthority, err := st.YTAuthorityOf(ctx, store.YTAuthorityComments)
	if err != nil {
		return report, err
	}
	taskAuthority, err := st.YTAuthorityOf(ctx, store.YTAuthorityTasks)
	if err != nil {
		return report, err
	}
	for afterID := int64(0); ; {
		ops, err := st.ListYTWriteOps(ctx, store.YTOpFilter{State: "pending", TaskID: taskID, AfterID: afterID, Limit: store.YTOpListMax})
		if err != nil {
			return report, err
		}
		for _, op := range ops {
			afterID = op.ID
			if op.Tenant != tenant || !ytBindingDrainOp(op) {
				continue
			}
			binding, found, err := st.YTTaskBinding(ctx, parseTaskID(op.EntityID))
			if err != nil {
				return report, err
			}
			modeOn, side := ytBindingDrainModeAndAuthority(op, comments, fields, moves, commentAuthority, taskAuthority)
			class := ytBindingDrainClass(op, binding, found, tenant, modeOn, side)
			report.Counts[class]++
		}
		if len(ops) < store.YTOpListMax {
			break
		}
	}
	return report, nil
}

func parseTaskID(raw string) int64 {
	id, _ := strconv.ParseInt(raw, 10, 64)
	return id
}

func ytBindingDrainOp(op ytwrite.Op) bool {
	return (op.Kind == ytwrite.KindCommentAppend && op.Principal == store.YTCommentPrincipal) ||
		((op.Kind == ytwrite.KindIssueFields || op.Kind == ytwrite.KindIssueMove) && op.Principal == store.YTFieldsPrincipal)
}

func ytBindingDrainModeAndAuthority(op ytwrite.Op, comments store.YTCommentsMode, fields store.YTFieldsMode, moves store.YTMovesMode, commentAuthority, taskAuthority store.YTAuthoritySide) (bool, store.YTAuthoritySide) {
	if op.Kind == ytwrite.KindCommentAppend {
		return comments == store.YTCommentsOn, commentAuthority
	}
	return fields == store.YTFieldsOn && (op.Kind != ytwrite.KindIssueMove || moves == store.YTMovesOn), taskAuthority
}

func ytBindingDrainClass(op ytwrite.Op, binding store.YTTaskBinding, found bool, tenant string, modeOn bool, authority store.YTAuthoritySide) string {
	switch op.State {
	case ytwrite.StateRemoteUnknown:
		return "remote_unknown"
	case ytwrite.StateDispatched:
		return "ready"
	}
	if !modeOn {
		return "held_mode"
	}
	if op.YTTargetID == "" {
		return "ready"
	}
	if found && binding.Tenant != tenant {
		return "held_other_tenant"
	}
	if (!found || (op.YTTargetID != "" && binding.IssueID != op.YTTargetID)) && authority == store.YTAuthorityYouTrack {
		return "held_authority_youtrack"
	}
	if !found {
		return "held_binding"
	}
	if op.YTTargetID != "" && binding.IssueID != op.YTTargetID {
		return "held_binding_changed"
	}
	return "ready"
}

const youtrackIssuesBackfillUsage = "usage: youtrack issues-backfill [--project P] [--dry-run]"

// youtrackIssuesBackfillCmd is `youtrack issues-backfill` (hk 3898): it
// prepares the issue_create op of every task with no binding, bound to the
// tenant of YOUTRACK_URL (the host the write worker sends to), and calls no
// YouTrack: adopting an issue that already carries the task's hk ID happens
// when the op is sent. A task that already has its op is left alone, so a
// second run prepares nothing. --dry-run counts without writing. It opens the
// database directly, like `youtrack canary`.
func youtrackIssuesBackfillCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("youtrack issues-backfill", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	project := fs.String("project", "", "only tasks of this hk project")
	dryRun := fs.Bool("dry-run", false, "count the tasks without preparing ops")
	ordered, err := splitArgs(args, "project")
	if err != nil {
		return err
	}
	if err := fs.Parse(ordered); err != nil {
		return fmt.Errorf("%v; %s", err, youtrackIssuesBackfillUsage)
	}
	if fs.NArg() != 0 {
		return errors.New(youtrackIssuesBackfillUsage)
	}
	dbURL := strings.TrimSpace(os.Getenv("HANDOFFKEEP_DB_URL"))
	baseURL := strings.TrimSpace(os.Getenv("YOUTRACK_URL"))
	token := strings.TrimSpace(os.Getenv("YOUTRACK_BOT_API_KEY"))
	if dbURL == "" || baseURL == "" || token == "" {
		return exitCodeError{code: youtrackExitConfig, err: errors.New("HANDOFFKEEP_DB_URL, YOUTRACK_URL and YOUTRACK_BOT_API_KEY are required")}
	}
	// The client names the tenant and resolves the no-project import bucket.
	client, err := youtrack.New(youtrack.Config{BaseURL: baseURL, Token: token, UserAgent: "handoffkeep-ytwrite"})
	if err != nil {
		return exitCodeError{code: youtrackExitConfig, err: err}
	}
	tenant := youtrack.NewWriteRemote(client).Tenant()
	openCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, err := store.Open(openCtx, dbURL)
	if err != nil {
		return err
	}
	defer st.Close()
	bucketID, err := youtrack.ResolveBucketProject(openCtx, client)
	if err != nil {
		return exitCodeError{code: youtrackExitConfig, err: fmt.Errorf("resolve the %s bucket project: %w", youtrack.BucketShortName, err)}
	}
	st.SetYTIssuesBucketProject(bucketID)
	report, err := st.BackfillYTIssueOps(context.Background(), tenant, strings.TrimSpace(*project), *dryRun)
	if err != nil {
		return err
	}
	return printJSON(out, report)
}

const youtrackFieldsBackfillUsage = "usage: youtrack fields-backfill [--project P] [--task N] [--limit N] [--dry-run]"

// youtrackFieldsBackfillCmd is `youtrack fields-backfill` (hk 4022): it
// prepares the issue_fields snapshot of every bound task whose snapshot
// differs from the last one prepared, on the tenant of YOUTRACK_URL, by the
// task_fields consumer's rule, and calls no YouTrack: serve's write worker
// sends the ops. A second run prepares nothing. --limit caps the ops one run
// prepares; --dry-run counts without writing. It refuses with
// youtrackExitFieldsOff unless HANDOFFKEEP_YT_FIELDS_MODE is on, and opens
// the database directly, like `youtrack issues-backfill`.
func youtrackFieldsBackfillCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("youtrack fields-backfill", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	project := fs.String("project", "", "only tasks of this hk project")
	task := fs.Int64("task", 0, "only this task")
	limit := fs.Int("limit", 0, "maximum ops prepared in this run (0: no cap)")
	dryRun := fs.Bool("dry-run", false, "count the tasks without preparing ops")
	ordered, err := splitArgs(args, "project", "task", "limit")
	if err != nil {
		return err
	}
	if err := fs.Parse(ordered); err != nil {
		return fmt.Errorf("%v; %s", err, youtrackFieldsBackfillUsage)
	}
	if fs.NArg() != 0 || *task < 0 || *limit < 0 {
		return errors.New(youtrackFieldsBackfillUsage)
	}
	mode, err := ytFieldsModeFromEnv()
	if err != nil {
		return exitCodeError{code: youtrackExitConfig, err: err}
	}
	if mode != store.YTFieldsOn {
		return exitCodeError{code: youtrackExitFieldsOff, err: errors.New("fields-backfill needs HANDOFFKEEP_YT_FIELDS_MODE=on (the write worker admits no issue_fields op while it is off)")}
	}
	dbURL := strings.TrimSpace(os.Getenv("HANDOFFKEEP_DB_URL"))
	baseURL := strings.TrimSpace(os.Getenv("YOUTRACK_URL"))
	token := strings.TrimSpace(os.Getenv("YOUTRACK_BOT_API_KEY"))
	if dbURL == "" || baseURL == "" || token == "" {
		return exitCodeError{code: youtrackExitConfig, err: errors.New("HANDOFFKEEP_DB_URL, YOUTRACK_URL and YOUTRACK_BOT_API_KEY are required")}
	}
	// The client only names the tenant here; nothing is sent.
	client, err := youtrack.New(youtrack.Config{BaseURL: baseURL, Token: token, UserAgent: "handoffkeep-ytwrite"})
	if err != nil {
		return exitCodeError{code: youtrackExitConfig, err: err}
	}
	tenant := youtrack.NewWriteRemote(client).Tenant()
	openCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, err := store.Open(openCtx, dbURL)
	if err != nil {
		return err
	}
	defer st.Close()
	st.SetYTFieldsMode(mode, tenant)
	moves, err := ytMovesModeFromEnv()
	if err != nil {
		return exitCodeError{code: youtrackExitConfig, err: err}
	}
	st.SetYTMovesMode(moves)
	report, err := st.BackfillYTFieldsOps(context.Background(), store.YTFieldsBackfillOptions{
		Project: strings.TrimSpace(*project), TaskID: *task, Limit: *limit, DryRun: *dryRun})
	if err != nil {
		return err
	}
	return printJSON(out, report)
}
