// Package hubforward delivers notification_outbox rows to the panewire hub and
// records the hub's receipt, so a lane hears about an answer as it happens.
package hubforward

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mgh3326/handoffkeep/internal/store"
)

const (
	defaultInterval = 5 * time.Second
	postTimeout     = 5 * time.Second
	baseBackoff     = 30 * time.Second
	maxBackoff      = 10 * time.Minute
	batchSize       = 200
	// hubLaneTextMaxBytes is the current panewire relay ingress limit. Handoffkeep
	// stores the full outbox text; this cap applies only to the forwarded copy.
	hubLaneTextMaxBytes = 2048
	laneTextMarker      = "… [truncated; full text in hk]"
	// label is the hub's name for this sender.
	label = "hk-outbox"
)

// Store is the outbox surface the forwarder reads and marks.
type Store interface {
	ListUnsentNotifications(ctx context.Context, limit int, since time.Time) ([]store.NotificationOutbox, error)
	MarkNotificationSent(ctx context.Context, eventID string, hubRowID int64) (store.NotificationOutbox, error)
	SkipNotificationsBefore(ctx context.Context, before time.Time, reason string) (int64, error)
}

// notifyType is the lane-event type a row goes out with. A dots RESULT is
// ready (it does not interrupt), a QUESTION is decision and a BLOCKED is
// blocked (both interrupt; operator decision 166). A task order, or its
// withdrawal, is an order (hk 5125). A task drift delivery (hk 5112 track
// B-3) is info.
func notifyType(kind string) string {
	switch kind {
	case store.OutboxKindDotsDecision, store.OutboxKindDecisionAnswered:
		return "decision"
	case store.OutboxKindDotsBlocked:
		return "blocked"
	case store.OutboxKindDotsReport:
		return "ready"
	case store.OutboxKindTaskOrder:
		// An operator's order interrupts and is never digested (panewire
		// delivery_policy: class interrupt); the lane acts on the record.
		return "order"
	case store.OutboxKindTaskDrift:
		// The daily drift and hygiene delivery (hk 5112 track B-3) is a
		// digest, never an interrupt.
		return "info"
	case store.OutboxKindLaneCheck:
		// The per-lane wake check (operator decision 418, hk 5201) is a wake,
		// not an interrupt: the design session judges what to do on it.
		return "info"
	case store.OutboxKindDotsNote:
		// A dots note (hk 6307) is dots' opinion on a comment or intake, for the
		// design session to sort. It is info, never a wake-asking type.
		return "info"
	default:
		return "info"
	}
}

type retry struct {
	at    time.Time
	delay time.Duration
}

// errNoCutoff refuses to run without the instant forwarding started. A zero
// cutoff must not mean "send everything": that is exactly the backlog flush
// the cutoff exists to prevent (hk 4986).
var errNoCutoff = errors.New("notification forwarder has no cutoff")

// Forwarder posts unsent rows to the hub's relay ingress. The hub dedupes on
// lane and event id and returns the row id either way, so a retry after a lost
// response, a second forwarder, or a poller that sent the same event all end in
// one delivery and one receipt. One failing row does not hold up the others.
// Cutoff is required: rows created before it are settled as skipped, not sent,
// so the forwarder's first start never flushes the backlog (hk 4986).
type Forwarder struct {
	Store    Store
	URL      string
	Token    string
	HTTP     *http.Client
	Interval time.Duration
	Now      func() time.Time
	Cutoff   time.Time

	retries map[string]retry
}

// Run forwards due rows every interval until ctx ends. It settles the backlog
// before the cutoff once at start and logs what that start did; Once repeats
// the idempotent skip so a failed first attempt cannot let an old row through.
func (f *Forwarder) Run(ctx context.Context) {
	interval := f.Interval
	if interval <= 0 {
		interval = defaultInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	if f.Cutoff.IsZero() {
		log.Printf("notification_forward.disabled reason=no_cutoff")
		return
	}
	if skipped, err := f.Store.SkipNotificationsBefore(ctx, f.Cutoff, store.OutboxSkipBeforeForwarder); err != nil {
		log.Printf("notification_forward.skip_failed cutoff=%s err=%q", f.cutoffText(), err.Error())
	} else {
		log.Printf("notification_forward.start enabled=true cutoff=%s skipped=%d", f.cutoffText(), skipped)
	}
	for {
		if _, _, err := f.Once(ctx); err != nil && ctx.Err() == nil {
			log.Printf("notification_forward.list_error err=%q", err.Error())
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Once forwards every unsent row not waiting out a backoff. The list carries
// the cutoff, not only the settle, so a row that commits between the two reads
// can never be posted as backlog. A row counts as sent only once its receipt
// is stored; a failed mark goes round again and the hub's duplicate answer
// supplies the same id.
func (f *Forwarder) Once(ctx context.Context) (sent, failed int, err error) {
	if f.Cutoff.IsZero() {
		return 0, 0, errNoCutoff
	}
	if _, err := f.Store.SkipNotificationsBefore(ctx, f.Cutoff, store.OutboxSkipBeforeForwarder); err != nil {
		return 0, 0, fmt.Errorf("skip rows before cutoff: %w", err)
	}
	rows, err := f.Store.ListUnsentNotifications(ctx, batchSize, f.Cutoff)
	if err != nil {
		return 0, 0, err
	}
	if f.retries == nil {
		f.retries = map[string]retry{}
	}
	now := f.now()
	unsent := make(map[string]bool, len(rows))
	for _, row := range rows {
		unsent[row.EventID] = true
		if wait, waiting := f.retries[row.EventID]; waiting && now.Before(wait.at) {
			continue
		}
		if err := f.forward(ctx, row); err != nil {
			failed++
			delay := f.retries[row.EventID].delay * 2
			delay = max(delay, baseBackoff)
			delay = min(delay, maxBackoff)
			f.retries[row.EventID] = retry{at: now.Add(delay), delay: delay}
			log.Printf("notification_forward.failed event=%q err=%q", row.EventID, err.Error())
			continue
		}
		delete(f.retries, row.EventID)
		sent++
	}
	for id := range f.retries {
		if !unsent[id] {
			delete(f.retries, id)
		}
	}
	return sent, failed, nil
}

func (f *Forwarder) forward(ctx context.Context, row store.NotificationOutbox) error {
	id, err := f.post(ctx, row)
	if err != nil {
		return err
	}
	_, err = f.Store.MarkNotificationSent(ctx, row.EventID, id)
	return err
}

// post returns the hub row id from a 201, or from a 409 for an event already
// there. A duplicate with no id is not a receipt and stays a failure.
func (f *Forwarder) post(ctx context.Context, row store.NotificationOutbox) (int64, error) {
	payload, err := json.Marshal(struct {
		Kind       string `json:"kind"`
		Lane       string `json:"lane"`
		EventID    string `json:"event_id"`
		Text       string `json:"text"`
		Label      string `json:"label"`
		NotifyType string `json:"notify_type"`
	}{"lane.event", row.TargetLane, row.EventID, laneText(row.Text), label, notifyType(row.Kind)})
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, postTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(f.URL, "/")+"/v1/relay/events", bytes.NewReader(payload))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+f.Token)
	client := f.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusConflict {
		return 0, fmt.Errorf("hub status %d", resp.StatusCode)
	}
	var out struct {
		ID int64 `json:"id"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&out); err != nil {
		return 0, fmt.Errorf("hub response: %w", err)
	}
	if out.ID < 1 {
		return 0, errors.New("hub returned no row id")
	}
	return out.ID, nil
}

// laneText sanitizes and bounds only the forwarded copy. The durable outbox
// retains the full text so the hub's current limit cannot refuse or lose it.
func laneText(text string) string {
	text = strings.Map(func(r rune) rune {
		if r <= 0x1f || (r >= 0x7f && r <= 0x9f) {
			return ' '
		}
		return r
	}, text)
	if len(text) <= hubLaneTextMaxBytes {
		return text
	}
	keep := hubLaneTextMaxBytes - len(laneTextMarker)
	for keep > 0 && !utf8.ValidString(text[:keep]) {
		keep--
	}
	return text[:keep] + laneTextMarker
}

func (f *Forwarder) now() time.Time {
	if f.Now != nil {
		return f.Now().UTC()
	}
	return time.Now().UTC()
}

// cutoffText is the start log's rendering of the cutoff: one RFC3339 instant,
// as the operator set HANDOFFKEEP_NOTIFICATION_FORWARD_SINCE.
func (f *Forwarder) cutoffText() string {
	return f.Cutoff.UTC().Format(time.RFC3339)
}
