package tests

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mgh3326/handoffkeep/internal/store"
)

func chatPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("HANDOFFKEEP_TEST_DB_URL")
	if url == "" {
		t.Skip("HANDOFFKEEP_TEST_DB_URL is required for PostgreSQL chat tests")
	}
	p, err := pgxpool.New(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

func chatQuestionID(t *testing.T, n int) string {
	t.Helper()
	return fmt.Sprintf("Q-%s-%06d%02d", time.Now().UTC().Format("20060102"), time.Now().UnixNano()%1000000, n)
}

func putChatQuestion(t *testing.T, hURL, token, id, lane, body string) (int, store.ChatQuestion) {
	t.Helper()
	resp := request(t, http.DefaultClient, http.MethodPut, hURL+"/v1/chat/questions/"+id, token, map[string]string{"lane": lane, "body": body})
	defer resp.Body.Close()
	var got store.ChatQuestion
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, got
}

func TestChatQuestionUpsertKeepsOneRow(t *testing.T) {
	s := taskTestStore(t)
	h := taskHTTP(s)
	defer h.Close()
	lane := taskLane(t)
	id := chatQuestionID(t, 1)
	firstStatus, first := putChatQuestion(t, h.URL, "node-token", id, lane, "first body")
	secondStatus, second := putChatQuestion(t, h.URL, "node-token", id, lane, "second body")
	thirdStatus, third := putChatQuestion(t, h.URL, "node-token", id, lane, "third body")
	if firstStatus != http.StatusCreated || secondStatus != http.StatusOK || thirdStatus != http.StatusOK {
		t.Fatalf("statuses=%d,%d,%d", firstStatus, secondStatus, thirdStatus)
	}
	if first.ID != second.ID || second.ID != third.ID || third.Body != "third body" || third.State != "pending" {
		t.Fatalf("first=%+v second=%+v third=%+v", first, second, third)
	}
	got, err := s.ListChatQuestions(t.Context(), lane, "", "", 0)
	if err != nil || len(got) != 1 || got[0].Body != "third body" {
		t.Fatalf("questions=%+v err=%v", got, err)
	}
}

func TestChatQuestionValidation(t *testing.T) {
	s := taskTestStore(t)
	h := taskHTTP(s)
	defer h.Close()
	lane := taskLane(t)
	for _, id := range []string{"q-20260916-01", "Q-2026091-01", "Q-20260916", "Q-20260916-1"} {
		resp := request(t, h.Client(), http.MethodPut, h.URL+"/v1/chat/questions/"+id, "node-token", map[string]string{"lane": lane, "body": "x"})
		if resp.StatusCode != http.StatusBadRequest {
			resp.Body.Close()
			t.Fatalf("id=%q status=%d", id, resp.StatusCode)
		}
		resp.Body.Close()
	}
	resp := request(t, h.Client(), http.MethodPut, h.URL+"/v1/chat/questions/"+chatQuestionID(t, 2), "node-token", map[string]string{"lane": lane, "body": ""})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty body status=%d", resp.StatusCode)
	}
}

func TestChatQuestionTransitionPendingToResolved(t *testing.T) {
	s := taskTestStore(t)
	h := taskHTTP(s)
	defer h.Close()
	lane := taskLane(t)
	id := chatQuestionID(t, 3)
	if status, _ := putChatQuestion(t, h.URL, "node-token", id, lane, "needs an answer"); status != http.StatusCreated {
		t.Fatalf("put status=%d", status)
	}
	resp := request(t, h.Client(), http.MethodPost, h.URL+"/v1/chat/questions/"+id+"/transition", "node-token", map[string]string{"to": "resolved"})
	defer resp.Body.Close()
	var resolved store.ChatQuestion
	if err := json.NewDecoder(resp.Body).Decode(&resolved); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || resolved.State != "resolved" || resolved.ResolvedAt == nil {
		t.Fatalf("status=%d resolved=%+v", resp.StatusCode, resolved)
	}
	resp = request(t, h.Client(), http.MethodPost, h.URL+"/v1/chat/questions/"+id+"/transition", "node-token", map[string]string{"to": "withdrawn"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("resolved→withdrawn status=%d", resp.StatusCode)
	}
	resp = request(t, h.Client(), http.MethodPost, h.URL+"/v1/chat/questions/"+chatQuestionID(t, 99)+"/transition", "node-token", map[string]string{"to": "resolved"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing transition status=%d", resp.StatusCode)
	}
	resp = request(t, h.Client(), http.MethodPost, h.URL+"/v1/chat/questions/"+chatQuestionID(t, 4)+"/transition", "node-token", map[string]string{"to": "pending"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid transition status=%d", resp.StatusCode)
	}
}

func TestChatQuestionWithdrawnLeavesResolvedAtNull(t *testing.T) {
	s := taskTestStore(t)
	h := taskHTTP(s)
	defer h.Close()
	lane := taskLane(t)
	id := chatQuestionID(t, 5)
	putChatQuestion(t, h.URL, "node-token", id, lane, "withdraw me")
	resp := request(t, h.Client(), http.MethodPost, h.URL+"/v1/chat/questions/"+id+"/transition", "node-token", map[string]string{"to": "withdrawn"})
	defer resp.Body.Close()
	var withdrawn store.ChatQuestion
	if err := json.NewDecoder(resp.Body).Decode(&withdrawn); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || withdrawn.State != "withdrawn" || withdrawn.ResolvedAt != nil {
		t.Fatalf("status=%d withdrawn=%+v", resp.StatusCode, withdrawn)
	}
}

func TestChatQuestionsListPaginatesWithoutOverlap(t *testing.T) {
	s := taskTestStore(t)
	h := taskHTTP(s)
	defer h.Close()
	lane := taskLane(t)
	base := time.Now().UnixNano() % 1000000
	ids := []string{
		fmt.Sprintf("Q-%s-%07d", time.Now().UTC().Format("20060102"), base*10),
		fmt.Sprintf("Q-%s-%07d", time.Now().UTC().Format("20060102"), base*10+1),
		fmt.Sprintf("Q-%s-%07d", time.Now().UTC().Format("20060102"), base*10+2),
	}
	for i, id := range ids {
		if status, _ := putChatQuestion(t, h.URL, "node-token", id, lane, fmt.Sprintf("body %d", i)); status != http.StatusCreated {
			t.Fatalf("put %s status=%d", id, status)
		}
	}
	page := func(query string) []store.ChatQuestion {
		t.Helper()
		resp := request(t, h.Client(), http.MethodGet, h.URL+"/v1/chat/questions?lane="+lane+query, "node-token", nil)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("list status=%d", resp.StatusCode)
		}
		var listed struct {
			Questions []store.ChatQuestion `json:"questions"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil {
			t.Fatal(err)
		}
		return listed.Questions
	}
	first := page("&limit=2")
	second := page("&limit=2&after_id=" + first[len(first)-1].ID)
	if len(first) != 2 || len(second) != 1 {
		t.Fatalf("first=%v second=%v", first, second)
	}
	if first[0].ID != ids[0] || first[1].ID != ids[1] || second[0].ID != ids[2] {
		t.Fatalf("pages first=%v second=%v want %v", first, second, ids)
	}
	pending := page("&state=pending")
	if len(pending) != 3 {
		t.Fatalf("pending=%v", pending)
	}
	resp := request(t, h.Client(), http.MethodGet, h.URL+"/v1/chat/questions?state=bogus", "node-token", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bogus state status=%d", resp.StatusCode)
	}
	resp = request(t, h.Client(), http.MethodGet, h.URL+"/v1/chat/questions?after_id=bogus", "node-token", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bogus after_id status=%d", resp.StatusCode)
	}
}

func postChatMessage(t *testing.T, hURL, token string, body map[string]any) (int, store.ChatMessage) {
	t.Helper()
	resp := request(t, http.DefaultClient, http.MethodPost, hURL+"/v1/chat/messages", token, body)
	defer resp.Body.Close()
	var got store.ChatMessage
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, got
}

func TestChatMessageCreateStoredThenDelivered(t *testing.T) {
	s := taskTestStore(t)
	h := taskHTTP(s)
	defer h.Close()
	status, message := postChatMessage(t, h.URL, "node-token", map[string]any{"body": "answer one"})
	if status != http.StatusCreated || message.Author != "operator" || message.RelayState != "stored" || message.DeliveredAt != nil {
		t.Fatalf("status=%d message=%+v", status, message)
	}
	resp := request(t, h.Client(), http.MethodPost, fmt.Sprintf("%s/v1/chat/messages/%d/delivered", h.URL, message.ID), "node-token", nil)
	defer resp.Body.Close()
	var delivered store.ChatMessage
	if err := json.NewDecoder(resp.Body).Decode(&delivered); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || delivered.RelayState != "delivered" || delivered.DeliveredAt == nil {
		t.Fatalf("status=%d delivered=%+v", resp.StatusCode, delivered)
	}
	resp = request(t, h.Client(), http.MethodPost, fmt.Sprintf("%s/v1/chat/messages/%d/delivered", h.URL, message.ID), "node-token", nil)
	defer resp.Body.Close()
	var again store.ChatMessage
	if err := json.NewDecoder(resp.Body).Decode(&again); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || !again.DeliveredAt.Equal(*delivered.DeliveredAt) {
		t.Fatalf("idempotent delivered=%+v again=%+v", delivered, again)
	}
	resp = request(t, h.Client(), http.MethodPost, h.URL+"/v1/chat/messages/999999999/delivered", "node-token", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing delivered status=%d", resp.StatusCode)
	}
}

func TestChatMessageAuthorValidation(t *testing.T) {
	s := taskTestStore(t)
	h := taskHTTP(s)
	defer h.Close()
	if status, desk := postChatMessage(t, h.URL, "node-token", map[string]any{"author": "desk", "body": "desk note"}); status != http.StatusCreated || desk.Author != "desk" {
		t.Fatalf("desk status=%d message=%+v", status, desk)
	}
	for _, body := range []map[string]any{
		{"author": "director", "body": "x"},
		{"author": "operator", "body": ""},
	} {
		resp := request(t, h.Client(), http.MethodPost, h.URL+"/v1/chat/messages", "node-token", body)
		if resp.StatusCode != http.StatusBadRequest {
			resp.Body.Close()
			t.Fatalf("body=%v status=%d", body, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

func TestChatMessageFailedTransition(t *testing.T) {
	s := taskTestStore(t)
	h := taskHTTP(s)
	defer h.Close()
	_, message := postChatMessage(t, h.URL, "node-token", map[string]any{"body": "will fail"})
	resp := request(t, h.Client(), http.MethodPost, fmt.Sprintf("%s/v1/chat/messages/%d/failed", h.URL, message.ID), "node-token", nil)
	defer resp.Body.Close()
	var failed store.ChatMessage
	if err := json.NewDecoder(resp.Body).Decode(&failed); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || failed.RelayState != "failed" {
		t.Fatalf("status=%d failed=%+v", resp.StatusCode, failed)
	}
	resp = request(t, h.Client(), http.MethodPost, fmt.Sprintf("%s/v1/chat/messages/%d/failed", h.URL, message.ID), "node-token", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("failed→failed status=%d", resp.StatusCode)
	}
}

func TestChatMessagesListPaginatesWithoutOverlap(t *testing.T) {
	s := taskTestStore(t)
	h := taskHTTP(s)
	defer h.Close()
	created := []store.ChatMessage{}
	for i := range 3 {
		status, message := postChatMessage(t, h.URL, "node-token", map[string]any{"body": fmt.Sprintf("page body %d", i)})
		if status != http.StatusCreated {
			t.Fatalf("status=%d", status)
		}
		created = append(created, message)
	}
	resp := request(t, h.Client(), http.MethodGet, fmt.Sprintf("%s/v1/chat/messages?author=operator&limit=2&after_id=%d", h.URL, created[0].ID-1), "node-token", nil)
	defer resp.Body.Close()
	var first struct {
		Messages []store.ChatMessage `json:"messages"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&first); err != nil {
		t.Fatal(err)
	}
	resp = request(t, h.Client(), http.MethodGet, fmt.Sprintf("%s/v1/chat/messages?author=operator&limit=2&after_id=%d", h.URL, first.Messages[len(first.Messages)-1].ID), "node-token", nil)
	defer resp.Body.Close()
	var second struct {
		Messages []store.ChatMessage `json:"messages"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&second); err != nil {
		t.Fatal(err)
	}
	if len(first.Messages) != 2 || len(second.Messages) != 1 || second.Messages[0].ID != created[2].ID {
		t.Fatalf("first=%+v second=%+v created=%+v", first.Messages, second.Messages, created)
	}
	resp = request(t, h.Client(), http.MethodGet, h.URL+"/v1/chat/messages?author=boss", "node-token", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bogus author status=%d", resp.StatusCode)
	}
}

func TestChatCheckConstraintsRejectInvalidRows(t *testing.T) {
	taskTestStore(t)
	p := chatPool(t)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano()%100000000)
	if _, err := p.Exec(t.Context(), `INSERT INTO chat_questions(id,lane,body,state,created_at,updated_at) VALUES($1,'lane-a','x','bogus',now(),now())`, "Q-20260101-"+suffix[:2]); err == nil {
		t.Fatal("bogus state insert succeeded")
	}
	if _, err := p.Exec(t.Context(), `INSERT INTO chat_messages(author,body,relay_state,created_at) VALUES('director','x','stored',now())`); err == nil {
		t.Fatal("director author insert succeeded")
	}
	if _, err := p.Exec(t.Context(), `INSERT INTO chat_messages(author,body,relay_state,created_at) VALUES('operator','x','bogus',now())`); err == nil {
		t.Fatal("bogus relay_state insert succeeded")
	}
	if _, err := p.Exec(t.Context(), `INSERT INTO chat_questions(id,lane,body,state,created_at,updated_at) VALUES('X-20260101-01','lane-a','x','pending',now(),now())`); err == nil {
		t.Fatal("bad id format insert succeeded")
	}
}

func clearExpiredChatRows(t *testing.T, p *pgxpool.Pool) {
	t.Helper()
	for _, table := range []string{"chat_questions", "chat_messages"} {
		if _, err := p.Exec(t.Context(), `DELETE FROM `+table+` WHERE created_at < now() - interval '1 year'`); err != nil {
			t.Fatal(err)
		}
	}
}

func insertChatQuestion(t *testing.T, p *pgxpool.Pool, id, state, age string) {
	t.Helper()
	if _, err := p.Exec(t.Context(), `INSERT INTO chat_questions(id,lane,body,state,created_at,updated_at) VALUES($1,'lane-a','retention',$2,now()-$3::interval,now())`, id, state, age); err != nil {
		t.Fatal(err)
	}
}

func insertChatMessage(t *testing.T, p *pgxpool.Pool, body, relayState, age string) int64 {
	t.Helper()
	var id int64
	if err := p.QueryRow(t.Context(), `INSERT INTO chat_messages(author,body,relay_state,created_at,delivered_at) VALUES('operator',$1,$2,now()-$3::interval,CASE WHEN $2='delivered' THEN now()-$3::interval END) RETURNING id`, body, relayState, age).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func chatRowCount(t *testing.T, p *pgxpool.Pool, query string, arg any, want int) {
	t.Helper()
	var n int
	if err := p.QueryRow(t.Context(), query, arg).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != want {
		t.Fatalf("%s arg=%v rows=%d want %d", query, arg, n, want)
	}
}

func TestChatRetentionPreservesExpiredNonTerminalRows(t *testing.T) {
	s := taskTestStore(t)
	p := chatPool(t)
	clearExpiredChatRows(t, p)
	nano := time.Now().UnixNano()
	pendingID := fmt.Sprintf("Q-20260101-%d", nano)
	insertChatQuestion(t, p, pendingID, "pending", "1 year 1 day")
	prefix := fmt.Sprintf("keep-%d-", nano)
	storedID := insertChatMessage(t, p, prefix+"stored", "stored", "1 year 1 day")
	failedID := insertChatMessage(t, p, prefix+"failed", "failed", "1 year 1 day")
	deleted, err := s.PruneChat(t.Context(), store.ChatPruneMaxDelete)
	if err != nil || deleted != 0 {
		t.Fatalf("deleted=%d err=%v — non-terminal rows must never be pruned", deleted, err)
	}
	chatRowCount(t, p, `SELECT count(*) FROM chat_questions WHERE id=$1`, pendingID, 1)
	chatRowCount(t, p, `SELECT count(*) FROM chat_messages WHERE id=$1`, storedID, 1)
	chatRowCount(t, p, `SELECT count(*) FROM chat_messages WHERE id=$1`, failedID, 1)
}

func TestChatRetentionDeletesExpiredTerminalRows(t *testing.T) {
	s := taskTestStore(t)
	p := chatPool(t)
	clearExpiredChatRows(t, p)
	nano := time.Now().UnixNano()
	resolvedID := fmt.Sprintf("Q-20260101-%d", nano)
	withdrawnID := fmt.Sprintf("Q-20260101-%d", nano+1)
	insertChatQuestion(t, p, resolvedID, "resolved", "1 year 1 day")
	insertChatQuestion(t, p, withdrawnID, "withdrawn", "1 year 1 day")
	prefix := fmt.Sprintf("gone-%d-", nano)
	deliveredID := insertChatMessage(t, p, prefix+"delivered", "delivered", "1 year 1 day")
	deleted, err := s.PruneChat(t.Context(), store.ChatPruneMaxDelete)
	if err != nil || deleted != 3 {
		t.Fatalf("deleted=%d err=%v", deleted, err)
	}
	chatRowCount(t, p, `SELECT count(*) FROM chat_questions WHERE id=$1`, resolvedID, 0)
	chatRowCount(t, p, `SELECT count(*) FROM chat_questions WHERE id=$1`, withdrawnID, 0)
	chatRowCount(t, p, `SELECT count(*) FROM chat_messages WHERE id=$1`, deliveredID, 0)
}

func TestChatRetentionKeepsFreshRowsAnyState(t *testing.T) {
	s := taskTestStore(t)
	p := chatPool(t)
	clearExpiredChatRows(t, p)
	nano := time.Now().UnixNano()
	for i, state := range []string{"pending", "resolved", "withdrawn"} {
		insertChatQuestion(t, p, fmt.Sprintf("Q-20260101-%d", nano+int64(i)), state, "11 months")
	}
	prefix := fmt.Sprintf("fresh-%d-", nano)
	messageIDs := map[string]int64{}
	for _, state := range []string{"stored", "delivered", "failed"} {
		messageIDs[state] = insertChatMessage(t, p, prefix+state, state, "11 months")
	}
	deleted, err := s.PruneChat(t.Context(), store.ChatPruneMaxDelete)
	if err != nil || deleted != 0 {
		t.Fatalf("deleted=%d err=%v — fresh rows must survive regardless of state", deleted, err)
	}
	for i := range 3 {
		chatRowCount(t, p, `SELECT count(*) FROM chat_questions WHERE id=$1`, fmt.Sprintf("Q-20260101-%d", nano+int64(i)), 1)
	}
	for _, id := range messageIDs {
		chatRowCount(t, p, `SELECT count(*) FROM chat_messages WHERE id=$1`, id, 1)
	}
}

func TestChatRetentionLeavesExistingTablesAlone(t *testing.T) {
	s := taskTestStore(t)
	p := chatPool(t)
	nano := time.Now().UnixNano()
	var docID int64
	if err := p.QueryRow(t.Context(), `INSERT INTO documents(key,kind,body,sha256,created_by,created_at,updated_at) VALUES($1,'note','old doc','x','test',now()-interval '2 years',now()) RETURNING id`, fmt.Sprintf("retention-doc-%d", nano)).Scan(&docID); err != nil {
		t.Fatal(err)
	}
	var relayID int64
	if err := p.QueryRow(t.Context(), `INSERT INTO relay_events(kind,job_id,epoch,owner_lane,received_at) VALUES('job.escalate',$1,0,'lane-a',now()-interval '2 years') RETURNING id`, fmt.Sprintf("retention-job-%d", nano)).Scan(&relayID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PruneChat(t.Context(), store.ChatPruneMaxDelete); err != nil {
		t.Fatal(err)
	}
	chatRowCount(t, p, `SELECT count(*) FROM documents WHERE id=$1`, docID, 1)
	chatRowCount(t, p, `SELECT count(*) FROM relay_events WHERE id=$1`, relayID, 1)
}

func TestChatRetentionHonorsDeleteCap(t *testing.T) {
	s := taskTestStore(t)
	p := chatPool(t)
	clearExpiredChatRows(t, p)
	prefix := fmt.Sprintf("prunecap-%d-", time.Now().UnixNano())
	for i := range 10 {
		insertChatMessage(t, p, fmt.Sprintf("%s%d", prefix, i), "delivered", "1 year 1 day")
	}
	deleted, err := s.PruneChat(t.Context(), 4)
	if err != nil || deleted != 4 {
		t.Fatalf("deleted=%d err=%v", deleted, err)
	}
	var remaining int
	if err := p.QueryRow(t.Context(), `SELECT count(*) FROM chat_messages WHERE body LIKE $1`, prefix+"%").Scan(&remaining); err != nil || remaining != 6 {
		t.Fatalf("remaining=%d err=%v", remaining, err)
	}
	deleted, err = s.PruneChat(t.Context(), 4)
	if err != nil || deleted != 4 {
		t.Fatalf("second deleted=%d err=%v", deleted, err)
	}
	if err := p.QueryRow(t.Context(), `SELECT count(*) FROM chat_messages WHERE body LIKE $1`, prefix+"%").Scan(&remaining); err != nil || remaining != 2 {
		t.Fatalf("second remaining=%d err=%v", remaining, err)
	}
	if _, err := p.Exec(t.Context(), `DELETE FROM chat_messages WHERE body LIKE $1`, prefix+"%"); err != nil {
		t.Fatal(err)
	}
}

func TestChatRequireAuthentication(t *testing.T) {
	s := taskTestStore(t)
	h := taskHTTP(s)
	defer h.Close()
	requests := []struct {
		method string
		url    string
		body   any
	}{
		{http.MethodPut, h.URL + "/v1/chat/questions/" + chatQuestionID(t, 1), map[string]string{"lane": "lane-a", "body": "x"}},
		{http.MethodPost, h.URL + "/v1/chat/questions/" + chatQuestionID(t, 1) + "/transition", map[string]string{"to": "resolved"}},
		{http.MethodGet, h.URL + "/v1/chat/questions", nil},
		{http.MethodPost, h.URL + "/v1/chat/messages", map[string]string{"body": "x"}},
		{http.MethodGet, h.URL + "/v1/chat/messages", nil},
		{http.MethodPost, h.URL + "/v1/chat/messages/1/delivered", nil},
		{http.MethodPost, h.URL + "/v1/chat/messages/1/failed", nil},
	}
	for _, test := range requests {
		resp := request(t, h.Client(), test.method, test.url, "", test.body)
		if resp.StatusCode != http.StatusUnauthorized {
			resp.Body.Close()
			t.Fatalf("%s %s status=%d", test.method, test.url, resp.StatusCode)
		}
		resp.Body.Close()
	}
}
