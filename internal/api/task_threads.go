package api

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// Discord task threads (hk 3990): one forum post per task in the configured
// projects, then one short Korean line per record about it. The store owns
// what is due and the claim (store/task_threads.go); this file renders and
// sends. Push only: a webhook, no bot, nothing read back. See
// docs/task-threads.md.

const (
	taskThreadInterval = 10 * time.Second
	// taskThreadPerMinute is the most messages one webhook is sent a minute.
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
// only when the forum webhook is configured.
type TaskThreadPoster struct {
	Store   TaskThreadStore
	Discord *DiscordWebhook
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
		res := p.send(ctx, x)
		gaveUp, err := p.Store.SettleTaskThread(ctx, p.now(), x, res)
		logTaskThread(x, res, gaveUp)
		if err != nil {
			log.Printf("discord.task_thread_settle_failed task_id=%d key=%q err=%q", x.TaskID, x.Key, err.Error())
			return
		}
		if res.Outcome == store.TaskThreadRetry && res.RetryAfter > 0 {
			// A 429 is the webhook's limit, not one row's: everything waits.
			if p.sleep(ctx, res.RetryAfter) != nil {
				return
			}
		}
	}
}

// logTaskThread logs one settled send. The event names are spelled out so
// each stays greppable.
func logTaskThread(x store.TaskThreadItem, res store.TaskThreadResult, gaveUp bool) {
	errText := ""
	if res.Err != nil {
		errText = res.Err.Error()
	}
	create := x.Type == store.TaskThreadCreate
	switch {
	case res.Outcome == store.TaskThreadSent && create:
		log.Printf("discord.task_thread_created task_id=%d thread_id=%s", x.TaskID, res.ThreadID)
	case res.Outcome == store.TaskThreadSent:
	case res.Outcome == store.TaskThreadUnknown && create:
		log.Printf("discord.task_thread_needs_operator task_id=%d err=%q", x.TaskID, errText)
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

// send makes the one request the item needs and classifies its outcome.
func (p *TaskThreadPoster) send(ctx context.Context, x store.TaskThreadItem) store.TaskThreadResult {
	var users []string
	mention := ""
	if x.Mention && p.OperatorUserID != "" {
		users, mention = []string{p.OperatorUserID}, "<@"+p.OperatorUserID+"> "
	}
	var message struct {
		ID        string `json:"id"`
		ChannelID string `json:"channel_id"`
	}
	payload := map[string]any{"flags": discordSuppressEmbeds}
	var target string
	var err error
	if x.Type == store.TaskThreadCreate {
		payload["content"] = taskThreadStartText(x, p.ConsoleURL)
		payload["thread_name"] = x.ThreadName
		target, err = p.Discord.endpoint("", true)
	} else {
		payload["content"] = clipRunes(mention+taskThreadLineText(x, p.ConsoleURL), discordContentMaxRunes)
		target, err = p.Discord.threadEndpoint(x.ThreadID)
	}
	if err != nil {
		return store.TaskThreadResult{Outcome: store.TaskThreadRetry, Err: err}
	}
	err = p.Discord.sendPayload(ctx, http.MethodPost, target, payload, &message, users...)
	if err == nil && !discordMessageIDRE.MatchString(message.ID) {
		err = errDiscordNoMessageID
	}
	res := store.TaskThreadResult{Outcome: taskThreadOutcome(err), Err: err}
	var status discordStatusError
	if errors.As(err, &status) {
		res.RetryAfter = status.retryAfter
	}
	if res.Outcome == store.TaskThreadSent && x.Type == store.TaskThreadCreate {
		// A forum post's message lives in the thread it created.
		if !discordMessageIDRE.MatchString(message.ChannelID) {
			return store.TaskThreadResult{Outcome: store.TaskThreadUnknown, Err: errors.New("discord webhook returned no thread id")}
		}
		res.ThreadID = message.ChannelID
	}
	return res
}

// threadEndpoint is the webhook URL that posts into one thread and waits for
// the message, so a 2xx means the line exists.
func (d DiscordWebhook) threadEndpoint(threadID string) (string, error) {
	if !discordMessageIDRE.MatchString(threadID) {
		return "", errors.New("invalid discord thread id")
	}
	target, err := d.endpoint("", true)
	if err != nil {
		return "", err
	}
	parsed, err := url.Parse(target)
	if err != nil {
		return "", errors.New("discord webhook url could not be parsed")
	}
	query := parsed.Query()
	query.Set("thread_id", threadID)
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

// taskThreadOutcome retries only what Discord certainly did not take: a 429
// or 5xx answer, or a connection never made. A 4xx is a refusal a retry
// cannot fix. Anything else (timeout, reset, a 2xx whose body could not be
// read) may have created the message, so it is never sent again.
func taskThreadOutcome(err error) store.TaskThreadOutcome {
	if err == nil {
		return store.TaskThreadSent
	}
	var status discordStatusError
	var op *net.OpError
	switch {
	case errors.As(err, &status) && (status.status == http.StatusTooManyRequests || status.status >= 500):
		return store.TaskThreadRetry
	case errors.As(err, &status):
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
