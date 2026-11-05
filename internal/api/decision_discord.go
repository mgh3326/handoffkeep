package api

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// Every decision request is announced in the decisions channel, and its
// message is edited when the request closes; once a day a digest lists the
// requests still open after a day. The store owns the outbox
// (store/decision_notify.go); this file sends it. See docs/decision-discord.md.

const decisionNotifyInterval = 10 * time.Second

// discordMessageIDRE is a Discord snowflake. A message id goes into the edit
// URL's path, so nothing else is accepted.
var discordMessageIDRE = regexp.MustCompile(`^[0-9]{1,32}$`)

// errDiscordNoMessageID is a post Discord accepted without returning the
// message id, so the message can never be edited.
var errDiscordNoMessageID = errors.New("discord webhook returned no message id")

// endpoint is the webhook URL with suffix added to its path and its query
// kept (a thread_id must reach the edit too). The error never quotes the URL.
func (d DiscordWebhook) endpoint(suffix string, wait bool) (string, error) {
	parsed, err := url.Parse(d.URL)
	if err != nil {
		return "", errors.New("discord webhook url could not be parsed")
	}
	parsed.Path = strings.TrimSuffix(parsed.Path, "/") + suffix
	parsed.RawPath = ""
	if wait {
		query := parsed.Query()
		query.Set("wait", "true")
		parsed.RawQuery = query.Encode()
	}
	return parsed.String(), nil
}

// PostMessage sends one message and returns its id, which EditMessage needs.
// ?wait=true makes Discord answer with the message instead of 204.
func (d DiscordWebhook) PostMessage(ctx context.Context, content string) (string, error) {
	target, err := d.endpoint("", true)
	if err != nil {
		return "", err
	}
	var message struct {
		ID string `json:"id"`
	}
	if err := d.send(ctx, http.MethodPost, target, content, &message); err != nil {
		return "", err
	}
	if !discordMessageIDRE.MatchString(message.ID) {
		return "", errDiscordNoMessageID
	}
	return message.ID, nil
}

// EditMessage replaces the content of a message this webhook posted.
func (d DiscordWebhook) EditMessage(ctx context.Context, messageID, content string) error {
	if !discordMessageIDRE.MatchString(messageID) {
		return errors.New("invalid discord message id")
	}
	target, err := d.endpoint("/messages/"+messageID, false)
	if err != nil {
		return err
	}
	return d.send(ctx, http.MethodPatch, target, content, nil)
}

// ParseConsoleURL accepts the console's https base URL, for the link in
// channel messages, and returns it without a trailing slash. The message says
// why without quoting the value.
func ParseConsoleURL(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("HANDOFFKEEP_CONSOLE_URL must be an https url with no query or fragment")
	}
	return strings.TrimRight(raw, "/"), nil
}

// DecisionNotifyStore is the store surface DecisionNotifier works through.
type DecisionNotifyStore interface {
	ProcessNextDecisionNotice(ctx context.Context, now time.Time, send func(context.Context, store.DecisionNotice) (string, error)) (bool, error, error)
	ProcessDecisionDigest(ctx context.Context, now time.Time, send func(context.Context, store.DecisionDigest) (string, error)) (bool, error, error)
}

// DecisionNotifier posts and edits the decision request messages and sends
// the daily digest. It runs whether or not the webhook is set: without one,
// what falls due settles as unsent, so setting it later replays nothing.
type DecisionNotifier struct {
	Store DecisionNotifyStore
	// Discord is the decisions channel. Nil sends nothing.
	Discord *DiscordWebhook
	// ConsoleURL is the console's base URL. Empty leaves the link out and
	// says where to look instead.
	ConsoleURL string
	Interval   time.Duration
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// Run drains due messages every interval until ctx ends.
func (n DecisionNotifier) Run(ctx context.Context) {
	interval := n.Interval
	if interval <= 0 {
		interval = decisionNotifyInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		n.Drain(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (n DecisionNotifier) now() time.Time {
	if n.Now != nil {
		return n.Now().UTC()
	}
	return time.Now().UTC()
}

// Drain sends every message now due, then the day's digest if it is owed.
func (n DecisionNotifier) Drain(ctx context.Context) {
	now := n.now()
	for ctx.Err() == nil {
		found, sendErr, err := n.Store.ProcessNextDecisionNotice(ctx, now, func(ctx context.Context, x store.DecisionNotice) (string, error) {
			return n.sendNotice(ctx, x, now)
		})
		logDecisionNotify("decision_notify", sendErr)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("decision_notify.drain_error err=%q", err.Error())
			}
			return
		}
		if !found {
			break
		}
	}
	if ctx.Err() != nil {
		return
	}
	_, sendErr, err := n.Store.ProcessDecisionDigest(ctx, now, func(ctx context.Context, d store.DecisionDigest) (string, error) {
		if n.Discord == nil {
			return "", store.ErrDecisionNoticeSkipped
		}
		id, err := n.Discord.PostMessage(ctx, decisionDigestText(d, n.ConsoleURL, now))
		return id, decisionPostError(err)
	})
	logDecisionNotify("decision_notify.digest", sendErr)
	if err != nil && ctx.Err() == nil {
		log.Printf("decision_notify.digest_error err=%q", err.Error())
	}
}

func logDecisionNotify(event string, sendErr error) {
	switch {
	case errors.Is(sendErr, store.ErrDecisionNoticeGaveUp):
		log.Printf("%s.gave_up err=%q", event, sendErr.Error())
	case sendErr != nil:
		log.Printf("%s.failed err=%q", event, sendErr.Error())
	}
}

func (n DecisionNotifier) sendNotice(ctx context.Context, x store.DecisionNotice, now time.Time) (string, error) {
	if n.Discord == nil {
		return "", store.ErrDecisionNoticeSkipped
	}
	text := decisionNoticeText(x, n.ConsoleURL, now)
	if x.Post() {
		id, err := n.Discord.PostMessage(ctx, text)
		return id, decisionPostError(err)
	}
	return "", decisionNoticeError(n.Discord.EditMessage(ctx, x.MessageID, text))
}

// decisionNoticeError marks the failures a retry cannot fix: Discord refusing
// the request (a 4xx other than 429, such as a deleted message or webhook) or
// a post whose message id did not come back, which a retry would post again.
func decisionNoticeError(err error) error {
	if err == nil {
		return nil
	}
	var status discordStatusError
	if errors.As(err, &status) && status.status >= 400 && status.status < 500 && status.status != http.StatusTooManyRequests {
		return fmt.Errorf("%w: %w", store.ErrDecisionNoticeRejected, err)
	}
	if errors.Is(err, errDiscordNoMessageID) {
		return fmt.Errorf("%w: %w", store.ErrDecisionNoticeRejected, err)
	}
	return err
}

// decisionPostError retries a first post only when Discord certainly did not
// create the message: it answered with a status (5xx or 429), or the
// connection was never made. A post whose request may have reached Discord
// without an answer coming back (timeout, reset, unreadable 2xx body) is not
// retried, since a retry could leave two messages for one request; the row
// settles as failed and the daily digest still lists the request.
func decisionPostError(err error) error {
	if err == nil {
		return nil
	}
	var status discordStatusError
	var op *net.OpError
	switch {
	case errors.As(err, &status), errors.Is(err, errDiscordNoMessageID):
		return decisionNoticeError(err)
	case errors.As(err, &op) && op.Op == "dial":
		return err
	}
	return fmt.Errorf("%w: post outcome unknown, not retried: %w", store.ErrDecisionNoticeRejected, err)
}

const (
	decisionTitleMaxRunes   = 200
	decisionAnswerMaxRunes  = 300
	decisionDigestLineRunes = 100
)

// clipRunes cuts s to at most n runes, marking the cut with an ellipsis.
func clipRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	if n < 1 {
		return ""
	}
	return string([]rune(s)[:n-1]) + "…"
}

func decisionTime(t time.Time) string {
	return t.In(linearDecisionDeadlineZone).Format("2006-01-02 15:04 MST")
}

// decisionAge is a short duration: 45m, 5h, 3d 4h.
func decisionAge(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", max(int(d/time.Minute), 0))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return fmt.Sprintf("%dd %dh", int(d/(24*time.Hour)), int(d%(24*time.Hour)/time.Hour))
}

// decisionLinkLine is the console link, or where to look without one.
func decisionLinkLine(console string, id int64) string {
	if console == "" {
		if id > 0 {
			return fmt.Sprintf("Find request #%d on the console's Decisions page or with `handoffkeep decisions list`.", id)
		}
		return "Find them on the console's Decisions page or with `handoffkeep decisions list`."
	}
	return console + "/ui/decisions"
}

// decisionContext is the source, lane and task of a request.
func decisionContext(x store.DecisionNotice) string {
	r := x.Record
	parts := []string{"source " + r.Source}
	if r.Lane != "" {
		parts = append(parts, "lane "+r.Lane)
	}
	if r.TaskID != nil {
		task := fmt.Sprintf("task #%d", *r.TaskID)
		if x.TaskTitle != "" {
			task += ": " + clipRunes(x.TaskTitle, decisionTitleMaxRunes)
		}
		parts = append(parts, task)
	}
	return strings.Join(parts, " · ")
}

// decisionNoticeText is the message for the request's current status. An
// open request shows the question, cut to fit Discord's limit; a closed one
// shows how it closed in place of the question.
func decisionNoticeText(x store.DecisionNotice, console string, now time.Time) string {
	r := x.Record
	link := decisionLinkLine(console, r.ID)
	ref := fmt.Sprintf("**Decision request #%d**", r.ID)
	var closed string
	switch r.Status {
	case store.DecisionStatusAnswered:
		closed = "✅ " + ref + " answered"
		if r.AnsweredBy != "" {
			closed += " by " + clipRunes(r.AnsweredBy, decisionTitleMaxRunes)
		}
		at := r.UpdatedAt
		if r.AnsweredAt != nil {
			at = *r.AnsweredAt
		}
		closed += " at " + decisionTime(at)
		if r.Answer != "" {
			closed += ": " + clipRunes(r.Answer, decisionAnswerMaxRunes)
		}
	case store.DecisionStatusSuperseded:
		closed = "♻️ " + ref + " superseded"
		if r.SupersededBy != "" {
			closed += " (" + clipRunes(r.SupersededBy, decisionTitleMaxRunes) + ")"
		}
		closed += " at " + decisionTime(r.UpdatedAt)
	case store.DecisionStatusExpired:
		closed = "⌛ " + ref + " expired unanswered"
		if r.ExpiresAt != nil {
			closed += " at " + decisionTime(*r.ExpiresAt)
		}
	}
	if closed != "" {
		return clipRunes(closed+"\n"+decisionContext(x)+"\n"+link, discordContentMaxRunes)
	}
	head := "❓ " + ref + " · " + decisionContext(x) + "\n"
	asked := fmt.Sprintf("Asked %s (<t:%d:R>), open %s", decisionTime(r.CreatedAt), r.CreatedAt.Unix(), decisionAge(now.Sub(r.CreatedAt)))
	if r.DueAt != nil {
		asked += " · due " + decisionTime(*r.DueAt)
	}
	tail := "\n" + asked + "\n" + link
	room := discordContentMaxRunes - utf8.RuneCountInString(head) - utf8.RuneCountInString(tail)
	return clipRunes(head+clipRunes(r.Question, max(room, 0))+tail, discordContentMaxRunes)
}

// decisionDigestText lists the stale requests oldest first, one line each,
// and ends with "+N more" for those that did not fit in one message.
func decisionDigestText(d store.DecisionDigest, console string, now time.Time) string {
	head := fmt.Sprintf("🗓️ **Decision requests open over 24 hours** · %s (UTC) · %d", d.Day.Format(time.DateOnly), d.Total)
	link := decisionLinkLine(console, 0)
	var b strings.Builder
	b.WriteString(head)
	used := utf8.RuneCountInString(head) + 1 + utf8.RuneCountInString(link)
	shown := 0
	for _, x := range d.Items {
		r := x.Record
		question, _, _ := strings.Cut(strings.TrimSpace(r.Question), "\n")
		line := fmt.Sprintf("\n• #%d · open %s · %s: %s", r.ID, decisionAge(now.Sub(r.CreatedAt)), decisionContext(x), clipRunes(question, decisionDigestLineRunes))
		line = clipRunes(line, discordContentMaxRunes/4)
		// Keep room for the "+N more" line the rest would need.
		more := 0
		if rest := d.Total - shown - 1; rest > 0 {
			more = utf8.RuneCountInString(fmt.Sprintf("\n+%d more", rest))
		}
		if used+utf8.RuneCountInString(line)+more > discordContentMaxRunes {
			break
		}
		b.WriteString(line)
		used += utf8.RuneCountInString(line)
		shown++
	}
	if rest := d.Total - shown; rest > 0 {
		fmt.Fprintf(&b, "\n+%d more", rest)
	}
	b.WriteString("\n" + link)
	return b.String()
}
