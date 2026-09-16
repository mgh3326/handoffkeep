package store

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mgh3326/handoffkeep/internal/guard"
)

// ChatPruneMaxDelete bounds every retention run so a single execution can
// never empty a table at once.
const ChatPruneMaxDelete = 1000

var chatQuestionIDRE = regexp.MustCompile(`^Q-[0-9]{8}-[0-9]{2,}$`)
var chatQuestionStates = map[string]bool{"pending": true, "resolved": true, "withdrawn": true}
var chatAuthors = map[string]bool{"operator": true, "desk": true}

var (
	ErrChatQuestionNotFound = errors.New("chat_question_not_found")
	ErrChatQuestionConflict = errors.New("chat_question_conflict")
	ErrChatMessageNotFound  = errors.New("chat_message_not_found")
	ErrChatMessageConflict  = errors.New("chat_message_conflict")
)

// ChatQuestion is a desk session's durable question for the operator. The id
// is assigned by the producer (Q-YYYYMMDD-NN) and is the upsert key.
type ChatQuestion struct {
	ID         string     `json:"id"`
	Lane       string     `json:"lane"`
	Body       string     `json:"body"`
	State      string     `json:"state"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	ResolvedAt *time.Time `json:"resolved_at"`
}

// ChatMessage is one operator-side chat row. Delivery state is durable here
// rather than in the relay path so it survives restarts.
type ChatMessage struct {
	ID          int64      `json:"id"`
	Author      string     `json:"author"`
	Body        string     `json:"body"`
	RelayState  string     `json:"relay_state"`
	CreatedAt   time.Time  `json:"created_at"`
	DeliveredAt *time.Time `json:"delivered_at"`
}

const chatQuestionColumns = `id,lane,body,state,created_at,updated_at,resolved_at`
const chatMessageColumns = `id,author,body,relay_state,created_at,delivered_at`

func scanChatQuestion(row interface{ Scan(...any) error }, x *ChatQuestion) error {
	return row.Scan(&x.ID, &x.Lane, &x.Body, &x.State, &x.CreatedAt, &x.UpdatedAt, &x.ResolvedAt)
}

func scanChatQuestionCreated(row interface{ Scan(...any) error }, x *ChatQuestion, created *bool) error {
	return row.Scan(&x.ID, &x.Lane, &x.Body, &x.State, &x.CreatedAt, &x.UpdatedAt, &x.ResolvedAt, created)
}

func scanChatMessage(row interface{ Scan(...any) error }, x *ChatMessage) error {
	return row.Scan(&x.ID, &x.Author, &x.Body, &x.RelayState, &x.CreatedAt, &x.DeliveredAt)
}

func validChatQuestion(x ChatQuestion) bool {
	return chatQuestionIDRE.MatchString(x.ID) && validName(x.Lane) && x.Body != "" && validText(x.Body, MaxBytes)
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
	now := time.Now().UTC()
	var created bool
	err := scanChatQuestionCreated(s.pool.QueryRow(ctx, `INSERT INTO chat_questions(id,lane,body,state,created_at,updated_at) VALUES($1,$2,$3,'pending',$4,$4) ON CONFLICT (id) DO UPDATE SET lane=EXCLUDED.lane,body=EXCLUDED.body,updated_at=EXCLUDED.updated_at RETURNING `+chatQuestionColumns+`,(xmax=0) AS created`, x.ID, x.Lane, x.Body, now), &x, &created)
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

// CreateChatMessage stores one chat row awaiting relay. The server owns the
// relay state: every new message starts 'stored' regardless of request input.
func (s *Store) CreateChatMessage(ctx context.Context, x ChatMessage) (ChatMessage, error) {
	if !chatAuthors[x.Author] || x.Body == "" || !validText(x.Body, MaxBytes) {
		return x, errors.New("invalid chat message")
	}
	if err := guard.Reject(x.Body); err != nil {
		return x, err
	}
	x.ID, x.RelayState, x.CreatedAt, x.DeliveredAt = 0, "stored", time.Now().UTC(), nil
	err := scanChatMessage(s.pool.QueryRow(ctx, `INSERT INTO chat_messages(author,body,relay_state,created_at) VALUES($1,$2,$3,$4) RETURNING `+chatMessageColumns, x.Author, x.Body, x.RelayState, x.CreatedAt), &x)
	if err != nil {
		return x, err
	}
	return x, nil
}

// MarkChatMessageDelivered records the first successful relay. Subsequent
// calls are intentionally idempotent and return the original delivery time.
func (s *Store) MarkChatMessageDelivered(ctx context.Context, id int64) (ChatMessage, error) {
	if id < 1 {
		return ChatMessage{}, errors.New("invalid chat message delivery")
	}
	var x ChatMessage
	err := scanChatMessage(s.pool.QueryRow(ctx, `UPDATE chat_messages SET relay_state='delivered',delivered_at=now() WHERE id=$1 AND relay_state='stored' RETURNING `+chatMessageColumns, id), &x)
	if err == nil {
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
	return x, nil
}

// MarkChatMessageFailed records a relay failure. Only stored rows can fail;
// delivered history is terminal.
func (s *Store) MarkChatMessageFailed(ctx context.Context, id int64) (ChatMessage, error) {
	if id < 1 {
		return ChatMessage{}, errors.New("invalid chat message failure")
	}
	var x ChatMessage
	err := scanChatMessage(s.pool.QueryRow(ctx, `UPDATE chat_messages SET relay_state='failed' WHERE id=$1 AND relay_state='stored' RETURNING `+chatMessageColumns, id), &x)
	if err == nil {
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
		q += " AND delivered_at IS NULL"
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
	return out, rows.Err()
}

// PruneChat deletes chat rows older than one year. Each table's delete is
// bounded by limit so one run can never empty a table at once; remaining
// expired rows are removed by subsequent runs.
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
	for _, table := range []string{"chat_messages", "chat_questions"} {
		tag, err := tx.Exec(ctx, `DELETE FROM `+table+` WHERE id IN (SELECT id FROM `+table+` WHERE created_at < now() - interval '1 year' ORDER BY created_at ASC, id ASC LIMIT $1)`, limit)
		if err != nil {
			return 0, err
		}
		total += tag.RowsAffected()
	}
	return total, tx.Commit(ctx)
}
