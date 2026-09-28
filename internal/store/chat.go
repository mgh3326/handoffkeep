package store

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mgh3326/handoffkeep/internal/guard"
)

// ChatPruneMaxDelete bounds every retention run so a single execution can
// never empty a table at once.
const ChatPruneMaxDelete = 1000
const ChatConversationID = "operator-desk"

var chatQuestionIDRE = regexp.MustCompile(`^Q-[0-9]{8}-[0-9]{2,}$`)
var chatQuestionStates = map[string]bool{"pending": true, "resolved": true, "withdrawn": true}
var chatMessageRelayStates = map[string]bool{"stored": true, "delivered": true, "failed": true, "not_sent": true}
var chatAuthors = map[string]bool{"operator": true, "desk": true}

// Terminal states are the only rows the retention job may delete once they
// pass the one-year mark. A pending question is still waiting for an operator
// answer, and a stored or failed message is an operator directive the fleet
// has not received — live operations data preserved regardless of age. A
// state added to the vocabulary above defaults to preserved unless listed
// here, so a new state can never silently fall into the delete path.
var chatQuestionTerminalStates = map[string]bool{"resolved": true, "withdrawn": true}
var chatMessageTerminalRelayStates = map[string]bool{"delivered": true}

// The question retention path uses only terminal question states. Message
// retention has a separate author predicate so desk history cannot enter it.
var chatPruneTables = []struct {
	table, stateColumn string
	terminal           map[string]bool
}{
	{"chat_questions", "state", chatQuestionTerminalStates},
}

func sortedStateKeys(states map[string]bool) []string {
	out := make([]string, 0, len(states))
	for s := range states {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

var (
	ErrChatQuestionNotFound = errors.New("chat_question_not_found")
	ErrChatQuestionConflict = errors.New("chat_question_conflict")
	ErrChatMessageNotFound  = errors.New("chat_message_not_found")
	ErrChatMessageConflict  = errors.New("chat_message_conflict")
	ErrChatConversation     = errors.New("chat_conversation_conflict")
)

// ChatQuestion is a desk session's durable question for the operator. The id
// is assigned by the producer (Q-YYYYMMDD-NN) and is the upsert key.
type ChatQuestion struct {
	ID             string     `json:"id"`
	ConversationID string     `json:"conversation_id"`
	Lane           string     `json:"lane"`
	Body           string     `json:"body"`
	State          string     `json:"state"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	ResolvedAt     *time.Time `json:"resolved_at"`
}

// ChatMessage is one operator-side chat row. Delivery state is durable here
// rather than in the relay path so it survives restarts.
type ChatMessage struct {
	ID                int64                  `json:"id"`
	ConversationID    string                 `json:"conversation_id"`
	Author            string                 `json:"author"`
	Body              string                 `json:"body"`
	SourceChannel     string                 `json:"source_channel"`
	OriginEventID     string                 `json:"origin_event_id"`
	OriginTimestamp   *time.Time             `json:"origin_timestamp"`
	QuestionRelations []ChatQuestionRelation `json:"question_relations"`
	RelayState        string                 `json:"relay_state"`
	CreatedAt         time.Time              `json:"created_at"`
	DeliveredAt       *time.Time             `json:"delivered_at"`
}

type ChatQuestionRelation struct {
	QuestionID   string `json:"question_id"`
	RelationKind string `json:"relation_kind"`
	QuestionText string `json:"question_text"`
}

type ChatMessagePost struct {
	Message              ChatMessage
	Questions            []ChatQuestion
	QuestionIDs          []string
	ProcessedQuestionIDs []string
}

const chatQuestionColumns = `id,conversation_id,lane,body,state,created_at,updated_at,resolved_at`
const chatMessageColumns = `id,conversation_id,author,body,source_channel,origin_event_id,origin_timestamp,relay_state,created_at,delivered_at`

func scanChatQuestion(row interface{ Scan(...any) error }, x *ChatQuestion) error {
	return row.Scan(&x.ID, &x.ConversationID, &x.Lane, &x.Body, &x.State, &x.CreatedAt, &x.UpdatedAt, &x.ResolvedAt)
}

func scanChatQuestionCreated(row interface{ Scan(...any) error }, x *ChatQuestion, created *bool) error {
	return row.Scan(&x.ID, &x.ConversationID, &x.Lane, &x.Body, &x.State, &x.CreatedAt, &x.UpdatedAt, &x.ResolvedAt, created)
}

func scanChatMessage(row interface{ Scan(...any) error }, x *ChatMessage) error {
	x.QuestionRelations = []ChatQuestionRelation{}
	return row.Scan(&x.ID, &x.ConversationID, &x.Author, &x.Body, &x.SourceChannel, &x.OriginEventID, &x.OriginTimestamp, &x.RelayState, &x.CreatedAt, &x.DeliveredAt)
}

func chatConversation(id string) (string, error) {
	if id == "" || id == ChatConversationID {
		return ChatConversationID, nil
	}
	return "", ErrChatConversation
}

func validChatQuestion(x ChatQuestion) bool {
	return (x.ConversationID == "" || x.ConversationID == ChatConversationID) && chatQuestionIDRE.MatchString(x.ID) && validName(x.Lane) && x.Body != "" && validText(x.Body, MaxBytes)
}

// UpsertChatQuestion inserts a question or refreshes an existing row with the
// same producer id. State and resolution time are owned by the transition
// path, so a repeated upsert never reopens or duplicates a row.
func (s *Store) UpsertChatQuestion(ctx context.Context, x ChatQuestion) (ChatQuestion, bool, error) {
	if !validChatQuestion(x) {
		return x, false, errors.New("invalid chat question")
	}
	if err := guard.Reject(x.Body); err != nil {
		return x, false, err
	}
	x.ConversationID = ChatConversationID
	now := time.Now().UTC()
	var created bool
	err := scanChatQuestionCreated(s.pool.QueryRow(ctx, `INSERT INTO chat_questions(id,conversation_id,lane,body,state,created_at,updated_at) VALUES($1,$2,$3,$4,'pending',$5,$5) ON CONFLICT (id) DO UPDATE SET lane=EXCLUDED.lane,body=EXCLUDED.body,updated_at=EXCLUDED.updated_at WHERE chat_questions.conversation_id=EXCLUDED.conversation_id RETURNING `+chatQuestionColumns+`,(xmax=0) AS created`, x.ID, x.ConversationID, x.Lane, x.Body, now), &x, &created)
	if errors.Is(err, pgx.ErrNoRows) {
		return x, false, ErrChatConversation
	}
	if err != nil {
		return x, false, err
	}
	return x, created, nil
}

func (s *Store) GetChatQuestion(ctx context.Context, id string) (ChatQuestion, bool, error) {
	if !chatQuestionIDRE.MatchString(id) {
		return ChatQuestion{}, false, errors.New("invalid chat question id")
	}
	var x ChatQuestion
	err := scanChatQuestion(s.pool.QueryRow(ctx, `SELECT `+chatQuestionColumns+` FROM chat_questions WHERE id=$1`, id), &x)
	if errors.Is(err, pgx.ErrNoRows) {
		return ChatQuestion{}, false, nil
	}
	if err != nil {
		return ChatQuestion{}, false, err
	}
	return x, true, nil
}

// TransitionChatQuestion moves a pending question to resolved or withdrawn.
// resolved_at records only an actual resolution; withdrawal leaves it NULL.
func (s *Store) TransitionChatQuestion(ctx context.Context, id, to string) (ChatQuestion, error) {
	if !chatQuestionIDRE.MatchString(id) || (to != "resolved" && to != "withdrawn") {
		return ChatQuestion{}, errors.New("invalid chat question transition")
	}
	now := time.Now().UTC()
	var x ChatQuestion
	err := scanChatQuestion(s.pool.QueryRow(ctx, `UPDATE chat_questions SET state=$2,resolved_at=CASE WHEN $2='resolved' THEN $3 ELSE resolved_at END,updated_at=$3 WHERE id=$1 AND state='pending' RETURNING `+chatQuestionColumns, id, to, now), &x)
	if err == nil {
		return x, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return ChatQuestion{}, err
	}
	if err = scanChatQuestion(s.pool.QueryRow(ctx, `SELECT `+chatQuestionColumns+` FROM chat_questions WHERE id=$1`, id), &x); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ChatQuestion{}, ErrChatQuestionNotFound
		}
		return ChatQuestion{}, err
	}
	return ChatQuestion{}, ErrChatQuestionConflict
}

// ListChatQuestions returns questions in id order, which matches creation
// order for the Q-YYYYMMDD-NN format. afterID is an exclusive cursor.
func (s *Store) ListChatQuestions(ctx context.Context, lane, state, afterID string, limit int) ([]ChatQuestion, error) {
	if (lane != "" && !validName(lane)) || (state != "" && !chatQuestionStates[state]) || (afterID != "" && !chatQuestionIDRE.MatchString(afterID)) {
		return nil, errors.New("invalid chat question query")
	}
	if limit < 1 {
		limit = 20
	}
	if limit > 1000 {
		limit = 1000
	}
	q, args := `SELECT `+chatQuestionColumns+` FROM chat_questions WHERE 1=1`, []any{}
	if lane != "" {
		args = append(args, lane)
		q += fmt.Sprintf(" AND lane=$%d", len(args))
	}
	if state != "" {
		args = append(args, state)
		q += fmt.Sprintf(" AND state=$%d", len(args))
	}
	if afterID != "" {
		args = append(args, afterID)
		q += fmt.Sprintf(" AND id>$%d", len(args))
	}
	args = append(args, limit)
	q += fmt.Sprintf(" ORDER BY id ASC LIMIT $%d", len(args))
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ChatQuestion{}
	for rows.Next() {
		var x ChatQuestion
		if err := scanChatQuestion(rows, &x); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// CreateChatMessage keeps the legacy store call working. The server chooses
// stored for operator messages and not_sent for desk messages.
func (s *Store) CreateChatMessage(ctx context.Context, x ChatMessage) (ChatMessage, error) {
	message, _, err := s.PostChatMessage(ctx, ChatMessagePost{Message: x})
	return message, err
}

// PostChatMessage commits the body, Q changes and relation snapshots together.
// The event-key lookup happens before any Q mutation, including on replay.
func (s *Store) PostChatMessage(ctx context.Context, post ChatMessagePost) (ChatMessage, bool, error) {
	m := post.Message
	var err error
	m.ConversationID, err = chatConversation(m.ConversationID)
	if err != nil {
		return ChatMessage{}, false, err
	}
	if m.SourceChannel == "" {
		m.SourceChannel = "legacy"
	}
	if !chatAuthors[m.Author] || m.Body == "" || !validText(m.Body, MaxBytes) ||
		(m.SourceChannel != "legacy" && m.SourceChannel != "web" && m.SourceChannel != "claude_stop") ||
		(m.SourceChannel == "web" && m.Author != "operator") ||
		(m.SourceChannel == "claude_stop" && m.Author != "desk") ||
		(m.OriginEventID != "" && (len(m.OriginEventID) > 256 || !validText(m.OriginEventID, 256) || strings.ContainsAny(m.OriginEventID, "\n\r"))) ||
		(m.SourceChannel != "legacy" && m.OriginEventID == "") ||
		len(post.Questions) > 1000 || len(post.QuestionIDs) > 1000 || len(post.ProcessedQuestionIDs) > 1000 ||
		(m.Author == "operator" && (len(post.Questions) != 0 || len(post.ProcessedQuestionIDs) != 0)) ||
		(m.Author == "desk" && len(post.QuestionIDs) != 0) {
		return ChatMessage{}, false, errors.New("invalid chat message")
	}
	if err := guard.Reject(m.Body); err != nil {
		return ChatMessage{}, false, err
	}
	for i := range post.Questions {
		if post.Questions[i].ConversationID != "" && post.Questions[i].ConversationID != m.ConversationID {
			return ChatMessage{}, false, ErrChatConversation
		}
		post.Questions[i].ConversationID = m.ConversationID
		if !validChatQuestion(post.Questions[i]) {
			return ChatMessage{}, false, errors.New("invalid chat question")
		}
		if err := guard.Reject(post.Questions[i].Body); err != nil {
			return ChatMessage{}, false, err
		}
	}
	for _, id := range append(append([]string{}, post.QuestionIDs...), post.ProcessedQuestionIDs...) {
		if !chatQuestionIDRE.MatchString(id) {
			return ChatMessage{}, false, errors.New("invalid chat question id")
		}
	}
	sort.Slice(post.Questions, func(i, j int) bool { return post.Questions[i].ID < post.Questions[j].ID })
	for i := 1; i < len(post.Questions); i++ {
		if post.Questions[i-1].ID == post.Questions[i].ID {
			return ChatMessage{}, false, errors.New("duplicate chat question")
		}
	}
	post.QuestionIDs = uniqueSorted(post.QuestionIDs)
	post.ProcessedQuestionIDs = uniqueSorted(post.ProcessedQuestionIDs)
	// Only the semantic fields enter the fingerprint. Current Q state and
	// timestamps cannot change what a prior event meant.
	type questionPayload struct{ ID, Lane, Body string }
	qs := make([]questionPayload, 0, len(post.Questions))
	for _, q := range post.Questions {
		qs = append(qs, questionPayload{q.ID, q.Lane, q.Body})
	}
	payload, _ := json.Marshal(struct {
		Author, Body                      string
		Questions                         []questionPayload
		QuestionIDs, ProcessedQuestionIDs []string
	}{m.Author, m.Body, qs, post.QuestionIDs, post.ProcessedQuestionIDs})
	hash := fmt.Sprintf("%x", sha256.Sum256(payload))
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ChatMessage{}, false, err
	}
	defer tx.Rollback(ctx)
	if m.OriginEventID != "" {
		key := strings.Join([]string{m.ConversationID, m.SourceChannel, m.OriginEventID}, "\x1f")
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1)::bigint)`, key); err != nil {
			return ChatMessage{}, false, err
		}
		var old ChatMessage
		var oldHash string
		err = scanChatMessageWithHash(tx.QueryRow(ctx, `SELECT `+chatMessageColumns+`,semantic_hash FROM chat_messages WHERE conversation_id=$1 AND source_channel=$2 AND origin_event_id=$3`, m.ConversationID, m.SourceChannel, m.OriginEventID), &old, &oldHash)
		if err == nil {
			if oldHash != hash {
				return ChatMessage{}, false, ErrChatMessageConflict
			}
			if err := tx.Commit(ctx); err != nil {
				return ChatMessage{}, false, err
			}
			if err := s.loadChatRelations(ctx, []*ChatMessage{&old}); err != nil {
				return ChatMessage{}, false, err
			}
			return old, false, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return ChatMessage{}, false, err
		}
	}
	now := time.Now().UTC()
	if m.Author == "desk" {
		m.RelayState = "not_sent"
	} else {
		m.RelayState = "stored"
	}
	m.ID, m.CreatedAt, m.DeliveredAt = 0, now, nil
	if err := scanChatMessage(tx.QueryRow(ctx, `INSERT INTO chat_messages(conversation_id,author,body,source_channel,origin_event_id,origin_timestamp,semantic_hash,relay_state,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING `+chatMessageColumns, m.ConversationID, m.Author, m.Body, m.SourceChannel, m.OriginEventID, m.OriginTimestamp, hash, m.RelayState, now), &m); err != nil {
		return ChatMessage{}, false, err
	}
	for _, q := range post.Questions {
		var ignored ChatQuestion
		err := scanChatQuestion(tx.QueryRow(ctx, `INSERT INTO chat_questions(id,conversation_id,lane,body,state,created_at,updated_at) VALUES($1,$2,$3,$4,'pending',$5,$5) ON CONFLICT (id) DO UPDATE SET lane=EXCLUDED.lane,body=EXCLUDED.body,updated_at=EXCLUDED.updated_at WHERE chat_questions.conversation_id=EXCLUDED.conversation_id RETURNING `+chatQuestionColumns, q.ID, m.ConversationID, q.Lane, q.Body, now), &ignored)
		if errors.Is(err, pgx.ErrNoRows) {
			return ChatMessage{}, false, ErrChatConversation
		}
		if err != nil {
			return ChatMessage{}, false, err
		}
	}
	addRelation := func(id, kind string) error {
		var conversation, body string
		err := tx.QueryRow(ctx, `SELECT conversation_id,body FROM chat_questions WHERE id=$1 FOR SHARE`, id).Scan(&conversation, &body)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrChatQuestionNotFound
		}
		if err != nil {
			return err
		}
		if conversation != m.ConversationID {
			return ErrChatConversation
		}
		_, err = tx.Exec(ctx, `INSERT INTO chat_message_questions(message_id,question_id,relation_kind,question_text) VALUES($1,$2,$3,$4)`, m.ID, id, kind, body)
		return err
	}
	for _, q := range post.Questions {
		if err := addRelation(q.ID, "posted"); err != nil {
			return ChatMessage{}, false, err
		}
	}
	for _, id := range post.QuestionIDs {
		if err := addRelation(id, "reply"); err != nil {
			return ChatMessage{}, false, err
		}
	}
	for _, id := range post.ProcessedQuestionIDs {
		if err := addRelation(id, "resolve"); err != nil {
			return ChatMessage{}, false, err
		}
		var state string
		if err := tx.QueryRow(ctx, `SELECT state FROM chat_questions WHERE id=$1`, id).Scan(&state); err != nil {
			return ChatMessage{}, false, err
		}
		if state == "withdrawn" {
			return ChatMessage{}, false, ErrChatQuestionConflict
		}
		if _, err := tx.Exec(ctx, `UPDATE chat_questions SET state='resolved',resolved_at=COALESCE(resolved_at,$2),updated_at=$2 WHERE id=$1 AND state='pending'`, id, now); err != nil {
			return ChatMessage{}, false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return ChatMessage{}, false, err
	}
	if err := s.loadChatRelations(ctx, []*ChatMessage{&m}); err != nil {
		return ChatMessage{}, false, err
	}
	return m, true, nil
}

func uniqueSorted(ids []string) []string {
	out := append([]string{}, ids...)
	sort.Strings(out)
	return slicesCompact(out)
}
func slicesCompact(ids []string) []string {
	if len(ids) == 0 {
		return ids
	}
	n := 1
	for _, id := range ids[1:] {
		if id != ids[n-1] {
			ids[n] = id
			n++
		}
	}
	return ids[:n]
}
func scanChatMessageWithHash(row interface{ Scan(...any) error }, x *ChatMessage, hash *string) error {
	x.QuestionRelations = []ChatQuestionRelation{}
	return row.Scan(&x.ID, &x.ConversationID, &x.Author, &x.Body, &x.SourceChannel, &x.OriginEventID, &x.OriginTimestamp, &x.RelayState, &x.CreatedAt, &x.DeliveredAt, hash)
}

func (s *Store) loadChatRelations(ctx context.Context, messages []*ChatMessage) error {
	if len(messages) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(messages))
	byID := make(map[int64]*ChatMessage, len(messages))
	for _, m := range messages {
		ids = append(ids, m.ID)
		byID[m.ID] = m
		m.QuestionRelations = []ChatQuestionRelation{}
	}
	rows, err := s.pool.Query(ctx, `SELECT message_id,question_id,relation_kind,question_text FROM chat_message_questions WHERE message_id=ANY($1) ORDER BY message_id,relation_kind,question_id`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var r ChatQuestionRelation
		if err := rows.Scan(&id, &r.QuestionID, &r.RelationKind, &r.QuestionText); err != nil {
			return err
		}
		byID[id].QuestionRelations = append(byID[id].QuestionRelations, r)
	}
	return rows.Err()
}

// MarkChatMessageDelivered records the first successful relay. Subsequent
// calls are intentionally idempotent and return the original delivery time.
func (s *Store) MarkChatMessageDelivered(ctx context.Context, id int64) (ChatMessage, error) {
	if id < 1 {
		return ChatMessage{}, errors.New("invalid chat message delivery")
	}
	var x ChatMessage
	err := scanChatMessage(s.pool.QueryRow(ctx, `UPDATE chat_messages SET relay_state='delivered',delivered_at=now() WHERE id=$1 AND author='operator' AND relay_state='stored' RETURNING `+chatMessageColumns, id), &x)
	if err == nil {
		if err := s.loadChatRelations(ctx, []*ChatMessage{&x}); err != nil {
			return ChatMessage{}, err
		}
		return x, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return ChatMessage{}, err
	}
	if err = scanChatMessage(s.pool.QueryRow(ctx, `SELECT `+chatMessageColumns+` FROM chat_messages WHERE id=$1`, id), &x); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ChatMessage{}, ErrChatMessageNotFound
		}
		return ChatMessage{}, err
	}
	if x.Author != "operator" {
		return ChatMessage{}, ErrChatMessageConflict
	}
	if err := s.loadChatRelations(ctx, []*ChatMessage{&x}); err != nil {
		return ChatMessage{}, err
	}
	return x, nil
}

// MarkChatMessageFailed records a relay failure. Only stored rows can fail;
// delivered history is terminal.
func (s *Store) MarkChatMessageFailed(ctx context.Context, id int64) (ChatMessage, error) {
	if id < 1 {
		return ChatMessage{}, errors.New("invalid chat message failure")
	}
	var x ChatMessage
	err := scanChatMessage(s.pool.QueryRow(ctx, `UPDATE chat_messages SET relay_state='failed' WHERE id=$1 AND author='operator' AND relay_state='stored' RETURNING `+chatMessageColumns, id), &x)
	if err == nil {
		if err := s.loadChatRelations(ctx, []*ChatMessage{&x}); err != nil {
			return ChatMessage{}, err
		}
		return x, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return ChatMessage{}, err
	}
	if err = scanChatMessage(s.pool.QueryRow(ctx, `SELECT `+chatMessageColumns+` FROM chat_messages WHERE id=$1`, id), &x); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ChatMessage{}, ErrChatMessageNotFound
		}
		return ChatMessage{}, err
	}
	return ChatMessage{}, ErrChatMessageConflict
}

// ListChatMessages returns messages in durable insertion order. afterID is an
// exclusive cursor; undelivered restricts to rows still awaiting relay.
func (s *Store) ListChatMessages(ctx context.Context, author string, undelivered bool, afterID int64, limit int) ([]ChatMessage, error) {
	if (author != "" && !chatAuthors[author]) || afterID < 0 {
		return nil, errors.New("invalid chat message query")
	}
	if limit < 1 {
		limit = 200
	}
	if limit > 1000 {
		limit = 1000
	}
	q, args := `SELECT `+chatMessageColumns+` FROM chat_messages WHERE 1=1`, []any{}
	if author != "" {
		args = append(args, author)
		q += fmt.Sprintf(" AND author=$%d", len(args))
	}
	if undelivered {
		q += " AND author='operator' AND relay_state='stored'"
	}
	if afterID > 0 {
		args = append(args, afterID)
		q += fmt.Sprintf(" AND id>$%d", len(args))
	}
	args = append(args, limit)
	q += fmt.Sprintf(" ORDER BY id ASC LIMIT $%d", len(args))
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ChatMessage{}
	for rows.Next() {
		var x ChatMessage
		if err := scanChatMessage(rows, &x); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	ptrs := make([]*ChatMessage, 0, len(out))
	for i := range out {
		ptrs = append(ptrs, &out[i])
	}
	if err := s.loadChatRelations(ctx, ptrs); err != nil {
		return nil, err
	}
	return out, nil
}

// PruneChat deletes terminal-state chat rows older than one year — resolved
// and withdrawn questions, delivered messages. Non-terminal rows (pending
// questions, stored or failed messages) are live operations data and are
// preserved regardless of age. Each table's delete is bounded by limit so one
// run can never empty a table at once; remaining expired rows are removed by
// subsequent runs.
func (s *Store) PruneChat(ctx context.Context, limit int) (int64, error) {
	if limit < 1 || limit > ChatPruneMaxDelete {
		limit = ChatPruneMaxDelete
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	var total int64
	for _, t := range chatPruneTables {
		tag, err := tx.Exec(ctx, `DELETE FROM `+t.table+` WHERE id IN (SELECT id FROM `+t.table+` WHERE created_at < now() - interval '1 year' AND `+t.stateColumn+` = ANY($1) ORDER BY created_at ASC, id ASC LIMIT $2)`, sortedStateKeys(t.terminal), limit)
		if err != nil {
			return 0, err
		}
		total += tag.RowsAffected()
	}
	// Desk rows are conversation history, not operator directives. The
	// directive retention rule only applies to delivered operator messages.
	tag, err := tx.Exec(ctx, `DELETE FROM chat_messages WHERE id IN (SELECT id FROM chat_messages WHERE author='operator' AND relay_state='delivered' AND created_at < now() - interval '1 year' ORDER BY created_at,id LIMIT $1)`, limit)
	if err != nil {
		return 0, err
	}
	total += tag.RowsAffected()
	return total, tx.Commit(ctx)
}
