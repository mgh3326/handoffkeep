package api

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// Discord task threads (hk 3990): one forum post per task in the configured
// projects, then one short Korean line per record about it. The store owns
// what is due and the claim (store/task_threads.go); this file renders and
// sends through the bot (discord_bot.go, hk 4040). It posts, and lists the
// forum's threads only to find a thread whose create answer was lost; it
// reads no message content. See docs/task-threads.md.

const (
	taskThreadInterval = 10 * time.Second
	// taskThreadPerMinute is the most messages the forum is sent a minute.
	taskThreadPerMinute = 30
	// discordSuppressEmbeds is Discord's SUPPRESS_EMBEDS message flag: a PR
	// link must not unfurl into the PR's description.
	discordSuppressEmbeds = 1 << 2
	taskThreadWhoMaxRunes = 100
)

// TaskThreadStore is the store surface TaskThreadPoster works through.
type TaskThreadStore interface {
	QueueTaskThreads(ctx context.Context, now time.Time, projects []string) (int, error)
	RecoverTaskThreadClaims(ctx context.Context, now time.Time) ([]store.TaskThreadRecovery, error)
	ClaimTaskThread(ctx context.Context, now time.Time) (store.TaskThreadItem, bool, error)
	SettleTaskThread(ctx context.Context, now time.Time, x store.TaskThreadItem, res store.TaskThreadResult) (bool, error)
}

// TaskThreadPoster creates the task threads and posts their lines. It runs
// only when the bot token and the forum are configured.
type TaskThreadPoster struct {
	Store   TaskThreadStore
	Discord TaskThreadDiscord
	// ForumID is the forum channel the threads are posts in.
	ForumID string
	// Projects are the task projects that get threads.
	Projects []string
	// ConsoleURL is the console's base URL. Empty leaves links out and says
	// where to look instead.
	ConsoleURL string
	// OperatorUserID is the Discord user a blocked review, a failed deploy
	// and a decision opened mention. Empty mentions nobody.
	OperatorUserID string
	Interval       time.Duration
	// PerMinute caps sends a minute; zero means taskThreadPerMinute.
	PerMinute int
	// Now is the clock; nil means time.Now. Sleep waits; nil uses a timer.
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error

	sent []time.Time
}

// Run drains due sends every interval until ctx ends.
func (p *TaskThreadPoster) Run(ctx context.Context) {
	interval := p.Interval
	if interval <= 0 {
		interval = taskThreadInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		p.Drain(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (p *TaskThreadPoster) now() time.Time {
	if p.Now != nil {
		return p.Now().UTC()
	}
	return time.Now().UTC()
}

func (p *TaskThreadPoster) sleep(ctx context.Context, d time.Duration) error {
	if p.Sleep != nil {
		return p.Sleep(ctx, d)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Drain queues the threads owed, settles claims a dead worker left, then
// sends everything due, oldest first, within the rate.
func (p *TaskThreadPoster) Drain(ctx context.Context) {
	if p.Discord == nil {
		return
	}
	if _, err := p.Store.QueueTaskThreads(ctx, p.now(), p.Projects); err != nil {
		if ctx.Err() == nil {
			log.Printf("discord.task_thread_queue_failed err=%q", err.Error())
		}
		return
	}
	recovered, err := p.Store.RecoverTaskThreadClaims(ctx, p.now())
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("discord.task_thread_recover_failed err=%q", err.Error())
		}
		return
	}
	for _, r := range recovered {
		log.Printf("discord.task_thread_claim_expired task_id=%d key=%q state=%s", r.TaskID, r.Key, r.State)
	}
	for ctx.Err() == nil {
		if err := p.waitRate(ctx); err != nil {
			return
		}
		x, found, err := p.Store.ClaimTaskThread(ctx, p.now())
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("discord.task_thread_claim_failed err=%q", err.Error())
			}
			return
		}
		if !found {
			return
		}
		res, recovered := p.send(ctx, x)
		gaveUp, err := p.Store.SettleTaskThread(ctx, p.now(), x, res)
		logTaskThread(x, res, recovered, gaveUp)
		if err != nil {
			log.Printf("discord.task_thread_settle_failed task_id=%d key=%q err=%q", x.TaskID, x.Key, err.Error())
			return
		}
		if res.Outcome == store.TaskThreadRetry && res.RetryAfter > 0 {
			// A 429 is the bot's limit, not one row's: everything waits.
			if p.sleep(ctx, res.RetryAfter) != nil {
				return
			}
		}
	}
}

// logTaskThread logs one settled send. The event names are spelled out so
// each stays greppable.
func logTaskThread(x store.TaskThreadItem, res store.TaskThreadResult, recovered, gaveUp bool) {
	errText := ""
	if res.Err != nil {
		errText = res.Err.Error()
	}
	create := x.Type == store.TaskThreadCreate
	switch {
	case res.Outcome == store.TaskThreadSent && recovered:
		log.Printf("discord.task_thread_recovered task_id=%d thread_id=%s", x.TaskID, res.ThreadID)
	case res.Outcome == store.TaskThreadSent && create:
		log.Printf("discord.task_thread_created task_id=%d thread_id=%s", x.TaskID, res.ThreadID)
	case res.Outcome == store.TaskThreadSent:
	case res.Outcome == store.TaskThreadAmbiguous, res.Outcome == store.TaskThreadUnknown && create && gaveUp:
		log.Printf("discord.task_thread_needs_operator task_id=%d err=%q", x.TaskID, errText)
	case res.Outcome == store.TaskThreadUnknown && create:
		log.Printf("discord.task_thread_create_unknown task_id=%d attempts=%d err=%q", x.TaskID, x.Attempts+1, errText)
	case create:
		log.Printf("discord.task_thread_create_failed task_id=%d gave_up=%t err=%q", x.TaskID, gaveUp, errText)
	default:
		log.Printf("discord.task_thread_post_failed task_id=%d key=%q type=%s gave_up=%t err=%q", x.TaskID, x.Key, x.Type, gaveUp, errText)
	}
}

// waitRate holds the next send until fewer than PerMinute went out in the
// last minute, then counts it.
func (p *TaskThreadPoster) waitRate(ctx context.Context) error {
	limit := p.PerMinute
	if limit <= 0 {
		limit = taskThreadPerMinute
	}
	for {
		now := p.now()
		kept := p.sent[:0]
		for _, at := range p.sent {
			if now.Sub(at) < time.Minute {
				kept = append(kept, at)
			}
		}
		p.sent = kept
		if len(p.sent) < limit {
			p.sent = append(p.sent, now)
			return nil
		}
		if err := p.sleep(ctx, p.sent[0].Add(time.Minute).Sub(now)); err != nil {
			return err
		}
	}
}

// send makes the request the item needs and classifies its outcome. A
// lookup item first searches the forum for the thread an earlier create may
// have made (decideLostCreate); recovered reports a thread adopted that way.
func (p *TaskThreadPoster) send(ctx context.Context, x store.TaskThreadItem) (res store.TaskThreadResult, recovered bool) {
	if x.Type != store.TaskThreadCreate {
		var users []string
		mention := ""
		if x.Mention && p.OperatorUserID != "" {
			users, mention = []string{p.OperatorUserID}, "<@"+p.OperatorUserID+"> "
		}
		msg := DiscordMessage{Content: clipRunes(mention+taskThreadLineText(x, p.ConsoleURL), discordContentMaxRunes), Flags: discordSuppressEmbeds, Users: users}
		return taskThreadResult(p.Discord.PostThreadMessage(ctx, x.ThreadID, msg)), false
	}
	if x.Lookup {
		botID, threads, err := p.Discord.ForumThreads(ctx, p.ForumID)
		lost := decideLostCreate(p.ForumID, x.ThreadName, botID, threads, err)
		switch lost.action {
		case lostCreateAdopt:
			return store.TaskThreadResult{Outcome: store.TaskThreadSent, ThreadID: lost.threadID}, true
		case lostCreateNeedsOperator:
			return store.TaskThreadResult{Outcome: store.TaskThreadAmbiguous, Err: lost.err}, false
		}
	}
	msg := DiscordMessage{Content: taskThreadStartText(x, p.ConsoleURL), Flags: discordSuppressEmbeds}
	threadID, err := p.Discord.CreateForumThread(ctx, p.ForumID, x.ThreadName, msg)
	res = taskThreadResult(err)
	if res.Outcome == store.TaskThreadSent {
		res.ThreadID = threadID
	}
	return res, false
}

// taskThreadResult classifies one request's error.
func taskThreadResult(err error) store.TaskThreadResult {
	res := store.TaskThreadResult{Outcome: taskThreadOutcome(err), Err: err}
	var status discordStatusError
	if errors.As(err, &status) {
		res.RetryAfter = status.retryAfter
	}
	return res
}

type lostCreateAction int

const (
	// lostCreateAdopt: exactly one thread is the lost create's; it is the
	// task's thread.
	lostCreateAdopt lostCreateAction = iota + 1
	// lostCreateCreate: no thread is; the create is made again.
	lostCreateCreate
	// lostCreateNeedsOperator: hk cannot tell (the lookup failed, or more
	// than one thread matches); the row stops for the operator.
	lostCreateNeedsOperator
)

type lostCreate struct {
	action   lostCreateAction
	threadID string
	err      error
}

// decideLostCreate decides what a create whose earlier outcome is unknown
// does, from the forum lookup. A candidate is a thread in forum with exactly
// the task's thread name that the bot itself owns: a thread someone else
// made, or one in another channel, is never the task's. The caller runs the
// lookup only once the unknown outcome is TaskThreadLookupDelay old, so a
// slow create has surfaced by then. One candidate is adopted, none means
// create again, and anything else (more than one, or no answer to the
// lookup) is the operator's: a second thread is never posted while one may
// exist.
func decideLostCreate(forumID, name, botUserID string, threads []DiscordThread, lookupErr error) lostCreate {
	if lookupErr != nil {
		return lostCreate{action: lostCreateNeedsOperator, err: fmt.Errorf("thread lookup failed: %w", lookupErr)}
	}
	if !discordMessageIDRE.MatchString(botUserID) {
		return lostCreate{action: lostCreateNeedsOperator, err: errors.New("thread lookup returned no bot user id")}
	}
	seen := map[string]bool{}
	var found []string
	for _, t := range threads {
		if t.ParentID != forumID || t.OwnerID != botUserID || t.Name != name || seen[t.ID] {
			continue
		}
		seen[t.ID] = true
		found = append(found, t.ID)
	}
	switch {
	case len(found) == 0:
		return lostCreate{action: lostCreateCreate}
	case len(found) > 1:
		return lostCreate{action: lostCreateNeedsOperator, err: fmt.Errorf("%d threads match the lost create", len(found))}
	case !discordMessageIDRE.MatchString(found[0]):
		return lostCreate{action: lostCreateNeedsOperator, err: errors.New("the matching thread has no valid id")}
	}
	return lostCreate{action: lostCreateAdopt, threadID: found[0]}
}

// taskThreadOutcome retries only what Discord certainly did not take: a 429
// (the rate limiter refuses before the request is handled) or a connection
// never made. Another 4xx is a refusal a retry cannot fix. Anything else
// may have created the message and is never sent again: a 5xx (a gateway
// can answer 502 after the post was made), a timeout or reset, a 2xx whose
// body could not be read.
func taskThreadOutcome(err error) store.TaskThreadOutcome {
	if err == nil {
		return store.TaskThreadSent
	}
	var status discordStatusError
	var op *net.OpError
	switch {
	case errors.As(err, &status) && status.status == http.StatusTooManyRequests:
		return store.TaskThreadRetry
	case errors.As(err, &status) && status.status < 500:
		return store.TaskThreadRejected
	case errors.As(err, &op) && op.Op == "dial":
		return store.TaskThreadRetry
	}
	return store.TaskThreadUnknown
}

// taskThreadTaskLink is the console page of the task, or where to look
// without one.
func taskThreadTaskLink(console string, id int64) string {
	if console == "" {
		return fmt.Sprintf("`handoffkeep tasks show %d`", id)
	}
	return fmt.Sprintf("%s/ui/tasks/%d", console, id)
}

// taskThreadStartText is a thread's first message: the task's structured
// fields and its link, never its body.
func taskThreadStartText(x store.TaskThreadItem, console string) string {
	parts := []string{fmt.Sprintf("📌 작업 #%d 시작", x.TaskID)}
	if x.Project != "" {
		parts = append(parts, "프로젝트 "+x.Project)
	}
	if x.Kind != "" {
		parts = append(parts, "종류 "+x.Kind)
	}
	if x.Grade != "" {
		parts = append(parts, "등급 "+x.Grade)
	}
	if x.Tier != "" {
		parts = append(parts, "티어 "+x.Tier)
	}
	return clipRunes(strings.Join(parts, " · ")+"\n"+taskThreadTaskLink(console, x.TaskID), discordContentMaxRunes)
}

// withLink appends ": <link>" when there is one.
func withLink(text, link string) string {
	if link == "" {
		return text
	}
	return text + ": " + link
}

// taskThreadWho is who answered, without the login an "operator:<email>"
// actor carries: the role is what the thread needs.
func taskThreadWho(by string) string {
	if role, _, ok := strings.Cut(by, ":"); ok && role == "operator" {
		return role
	}
	return clipRunes(by, taskThreadWhoMaxRunes)
}

// taskThreadLineText is one line, without its mention.
func taskThreadLineText(x store.TaskThreadItem, console string) string {
	switch x.Type {
	case store.TaskLinePROpened:
		return withLink("🔀 PR 열림", x.PR)
	case store.TaskLineReview:
		verdict := "✅ 리뷰 판정: 통과"
		if x.GateState == "blocked" {
			verdict = "⛔ 리뷰 판정: 차단"
		}
		f := x.Open
		counts := fmt.Sprintf("미해결 지적 critical %d · high %d · medium %d · low %d", f.Critical, f.High, f.Medium, f.Low)
		return withLink(verdict+" · "+counts, x.PR)
	case store.TaskLineMerged:
		return withLink("🟣 머지됨", x.PR)
	case store.TaskLineDeployed:
		return withLink("🚀 배포됨", x.PR)
	case store.TaskLineDeployFailed:
		return fmt.Sprintf("💥 배포 실패: %s (배포 #%d)", x.Service, x.DeployID)
	case store.TaskLineRolledBack:
		return fmt.Sprintf("↩️ 배포 롤백: %s (배포 #%d)", x.Service, x.DeployID)
	case store.TaskLineReverted:
		return withLink("⏪ 리버트됨", x.PR)
	case store.TaskLineDecisionOpened:
		return fmt.Sprintf("❓ 결정 요청 #%d 열림: %s", x.DecisionID, decisionLinkLine(console, x.DecisionID))
	case store.TaskLineDecisionAnswered:
		parts := []string{fmt.Sprintf("🗳️ 결정 요청 #%d 답변됨", x.DecisionID)}
		if who := taskThreadWho(x.AnsweredBy); who != "" {
			parts = append(parts, "답변자 "+who)
		}
		if x.Channel != "" {
			parts = append(parts, "경로 "+clipRunes(x.Channel, taskThreadWhoMaxRunes))
		}
		if !x.At.IsZero() {
			parts = append(parts, decisionTime(x.At))
		}
		return strings.Join(parts, " · ") + "\n" + decisionLinkLine(console, x.DecisionID)
	case store.TaskLineCompleted:
		return "🏁 완료"
	case store.TaskLineDropped:
		return "🗑️ 드롭됨"
	case store.TaskLineDuplicate:
		return "🔁 중복으로 종료"
	case store.TaskLineIcebox:
		return "🧊 보류(icebox)로 이동"
	}
	return "ℹ️ " + x.Type
}
