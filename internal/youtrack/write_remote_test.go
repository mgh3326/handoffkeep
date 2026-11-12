package youtrack

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
	"github.com/mgh3326/handoffkeep/internal/ytwrite/ytwritetest"
)

const writeRemoteToken = "tok-ytwrite-secret"

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "ytwrite", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// recordedYouTrack answers with recorded YouTrack JSON and counts requests.
type recordedYouTrack struct {
	mu       sync.Mutex
	requests []string
	bodies   []string
	answer   func(w http.ResponseWriter, r *http.Request)
	// transport, when set, wraps the client's transport.
	transport func(http.RoundTripper) http.RoundTripper
}

func (y *recordedYouTrack) serve(t *testing.T) *WriteRemote {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+writeRemoteToken {
			t.Errorf("authorization header = %q", r.Header.Get("Authorization"))
		}
		body, _ := io.ReadAll(r.Body)
		y.mu.Lock()
		y.requests = append(y.requests, r.Method+" "+r.URL.Path)
		y.bodies = append(y.bodies, string(body))
		y.mu.Unlock()
		y.answer(w, r)
	}))
	t.Cleanup(srv.Close)
	cfg := Config{BaseURL: srv.URL, Token: writeRemoteToken, Sleep: func(context.Context, time.Duration) error { return nil }}
	if y.transport != nil {
		cfg.HTTPClient = &http.Client{Transport: y.transport(http.DefaultTransport)}
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return NewWriteRemote(c)
}

func (y *recordedYouTrack) count() int { y.mu.Lock(); defer y.mu.Unlock(); return len(y.requests) }

// countOf is the number of requests with this method.
func (y *recordedYouTrack) countOf(method string) int {
	y.mu.Lock()
	defer y.mu.Unlock()
	n := 0
	for _, r := range y.requests {
		if strings.HasPrefix(r, method+" ") {
			n++
		}
	}
	return n
}

func TestAdapterCreateCommentDecodesRecordedReply(t *testing.T) {
	ctx := context.Background()
	y := &recordedYouTrack{answer: func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/issues/2-77/comments" || !strings.Contains(r.URL.Query().Get("fields"), "id") {
			t.Errorf("request %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
		_, _ = w.Write(fixture(t, "comment_create.json"))
	}}
	rm := y.serve(t)
	text := "작성: worker · 2026-11-09T12:00:00Z · hk op 17\n\n본문\n\n<!-- hk-op:17 -->"
	id, err := rm.CreateComment(ctx, "2-77", text)
	if err != nil || id != "4-31337" {
		t.Fatalf("CreateComment = %q, %v", id, err)
	}
	var sent struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(y.bodies[0]), &sent); err != nil || sent.Text != text {
		t.Fatalf("sent body %q (%v)", y.bodies[0], err)
	}
}

// A 2xx whose body has no usable id is uncertain: the adapter returns no
// trusted id and no error, and the protocol records remote_unknown.
func TestAdapterSuccessWithoutUsableIDIsUnknown(t *testing.T) {
	for name, body := range map[string][]byte{"no id": fixture(t, "comment_create_no_id.json"), "not json": []byte("<html>ok</html>"), "empty": nil, "bad id": []byte(`{"id":"../x y"}`)} {
		y := &recordedYouTrack{answer: func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body) }}
		rm := y.serve(t)
		id, err := rm.CreateComment(context.Background(), "2-77", "x")
		if err != nil || (id != "" && name != "bad id") {
			t.Errorf("%s: CreateComment = %q, %v; want an id the protocol will not trust and no error", name, id, err)
		}
		if op := driveOnce(t, rm); op.State != ytwrite.StateRemoteUnknown || op.Reason != ytwrite.ReasonNoID || op.YTResultID != "" {
			t.Errorf("%s: op %s/%q (result %q), want remote_unknown/%s", name, op.State, op.Reason, op.YTResultID, ytwrite.ReasonNoID)
		}
	}
}

// driveOnce sends one comment through the protocol over rm and returns the
// op as the ledger recorded it.
func driveOnce(t *testing.T, rm *WriteRemote) ytwrite.Op {
	t.Helper()
	ledger := ytwritetest.NewMemLedger()
	clock := ytwritetest.NewClock()
	p, err := ytwrite.New(ytwrite.Config{Ledger: ledger, Remote: rm, Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	r, err := p.AppendComment(context.Background(), ytwrite.CommentRequest{Principal: "hk:task-comments", Key: "task-comment:1", Actor: "worker", TaskID: 77, YTIssueID: "2-77", Body: "본문"})
	if err != nil {
		t.Fatal(err)
	}
	op, err := ledger.ByID(context.Background(), r.OpID)
	if err != nil {
		t.Fatal(err)
	}
	return op
}

// One POST per dispatch, and every observation lands in the state the
// protocol table names (docs/youtrack-write-protocol.md).
func TestAdapterErrorTranslation(t *testing.T) {
	type answer func(w http.ResponseWriter, r *http.Request)
	status := func(code int, body []byte) answer {
		return func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code); _, _ = w.Write(body) }
	}
	cases := []struct {
		name   string
		answer answer
		state  ytwrite.State
		reason string
	}{
		{"201 with id", status(http.StatusOK, fixture(t, "comment_create.json")), ytwrite.StateApplied, ""},
		{"200 without id", status(http.StatusOK, fixture(t, "comment_create_no_id.json")), ytwrite.StateRemoteUnknown, ytwrite.ReasonNoID},
		{"400", status(http.StatusBadRequest, nil), ytwrite.StateRejected, "http_400"},
		{"403", status(http.StatusForbidden, nil), ytwrite.StateRejected, "http_403"},
		{"404", status(http.StatusNotFound, fixture(t, "error_404.json")), ytwrite.StateRejected, "http_404"},
		{"409", status(http.StatusConflict, nil), ytwrite.StateRejected, "http_409"},
		{"422", status(http.StatusUnprocessableEntity, nil), ytwrite.StateRejected, "http_422"},
		{"401", status(http.StatusUnauthorized, nil), ytwrite.StatePrepared, ytwrite.ReasonUnauthorized},
		{"429", status(http.StatusTooManyRequests, nil), ytwrite.StatePrepared, ytwrite.ReasonRateLimited},
		{"408", status(http.StatusRequestTimeout, nil), ytwrite.StateRemoteUnknown, ytwrite.ReasonServerError},
		{"500", status(http.StatusInternalServerError, nil), ytwrite.StateRemoteUnknown, ytwrite.ReasonServerError},
		{"503", status(http.StatusServiceUnavailable, nil), ytwrite.StateRemoteUnknown, ytwrite.ReasonServerError},
		{"redirect is never replayed", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/api/issues/2-78/comments", http.StatusTemporaryRedirect)
		}, ytwrite.StateRemoteUnknown, ytwrite.ReasonTransport},
		{"connection dropped", func(w http.ResponseWriter, _ *http.Request) {
			conn, _, _ := w.(http.Hijacker).Hijack()
			_ = conn.Close()
		}, ytwrite.StateRemoteUnknown, ytwrite.ReasonTransport},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			y := &recordedYouTrack{answer: tc.answer}
			op := driveOnce(t, y.serve(t))
			if op.State != tc.state || op.Reason != tc.reason {
				t.Fatalf("op %s/%q, want %s/%q (last error %q)", op.State, op.Reason, tc.state, tc.reason, op.LastError)
			}
			if y.count() != 1 {
				t.Fatalf("%d requests %v, want exactly one POST", y.count(), y.requests)
			}
		})
	}
}

// 429 is answered once (no retry inside the call) and the server's wait, in
// seconds or as an HTTP date and however long, is what the op records.
func TestAdapter429IsOneAttemptAndDurablyDeferred(t *testing.T) {
	for name, value := range map[string]string{"seconds": "1200", "http date": time.Now().Add(20 * time.Minute).UTC().Format(http.TimeFormat)} {
		t.Run(name, func(t *testing.T) {
			y := &recordedYouTrack{answer: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Retry-After", value)
				w.WriteHeader(http.StatusTooManyRequests)
			}}
			rm := y.serve(t)
			_, err := rm.CreateComment(context.Background(), "2-77", "x")
			var re *ytwrite.RemoteError
			if !errors.As(err, &re) || re.Status != http.StatusTooManyRequests || re.RetryAfter < 19*time.Minute || y.count() != 1 {
				t.Fatalf("err %#v, %d requests", err, y.count())
			}
			op := driveOnce(t, rm)
			if op.State != ytwrite.StatePrepared || op.NextAttemptAt.Sub(op.CreatedAt) < 19*time.Minute {
				t.Fatalf("op %+v: the long wait was shortened", op)
			}
		})
	}
	t.Run("listing", func(t *testing.T) {
		y := &recordedYouTrack{answer: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "600")
			w.WriteHeader(http.StatusTooManyRequests)
		}}
		_, err := y.serve(t).ListComments(context.Background(), "2-77", 50)
		var re *ytwrite.RemoteError
		if !errors.As(err, &re) || re.Status != http.StatusTooManyRequests || re.RetryAfter != 10*time.Minute || y.count() != 1 {
			t.Fatalf("listing 429: %#v, %d requests", err, y.count())
		}
	})
}

// leakyBody returns its bytes and then a read error that quotes the bot key.
type leakyBody struct {
	io.Reader
	closer io.Closer
}

func (b leakyBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	if err == io.EOF {
		return n, errors.New("stream broke after token " + writeRemoteToken)
	}
	return n, err
}

func (b leakyBody) Close() error { return b.closer.Close() }

type leakyTransport struct{ next http.RoundTripper }

func (l leakyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := l.next.RoundTrip(r)
	if err == nil {
		resp.Body = leakyBody{Reader: resp.Body, closer: resp.Body}
	}
	return resp, err
}

// The bot key never reaches an error, so it never reaches the op log: not in
// an error body the server echoes, and not in a body read error.
func TestAdapterRedactsBeforePersistence(t *testing.T) {
	echo := []byte(`{"error":"invalid_grant","error_description":"token ` + writeRemoteToken + ` is not valid here"}`)
	y := &recordedYouTrack{answer: func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write(echo)
	}}
	rm := y.serve(t)
	_, err := rm.CreateComment(context.Background(), "2-77", "x")
	if err == nil || strings.Contains(err.Error(), writeRemoteToken) {
		t.Fatalf("err = %v", err)
	}
	op := driveOnce(t, rm)
	if op.State != ytwrite.StateRejected || strings.Contains(op.LastError, writeRemoteToken) || !strings.Contains(op.LastError, "[redacted]") {
		t.Fatalf("op %s last error %q", op.State, op.LastError)
	}

	t.Run("body read error", func(t *testing.T) {
		leaky := func(next http.RoundTripper) http.RoundTripper { return leakyTransport{next} }
		refused := &recordedYouTrack{transport: leaky, answer: func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"bad"}`))
		}}
		op := driveOnce(t, refused.serve(t))
		if op.State != ytwrite.StateRejected || strings.Contains(op.LastError, writeRemoteToken) || !strings.Contains(op.LastError, "[redacted]") {
			t.Fatalf("refused POST with a failing body: op %s last error %q", op.State, op.LastError)
		}
		// The evidence search reads a 200 whose body fails the same way.
		listing := &recordedYouTrack{transport: leaky, answer: func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			_, _ = w.Write([]byte(`[]`))
		}}
		ledger := ytwritetest.NewMemLedger()
		clock := ytwritetest.NewClock()
		p, err := ytwrite.New(ytwrite.Config{Ledger: ledger, Remote: listing.serve(t), Now: clock.Now})
		if err != nil {
			t.Fatal(err)
		}
		r, err := p.AppendComment(context.Background(), ytwrite.CommentRequest{Principal: "hk:task-comments", Key: "task-comment:2", Actor: "worker", TaskID: 77, YTIssueID: "2-77", Body: "본문"})
		if err != nil || r.State != ytwrite.StateRemoteUnknown {
			t.Fatalf("%+v %v", r, err)
		}
		clock.Advance(time.Minute)
		if _, err := p.Drive(context.Background(), 10); err != nil {
			t.Fatal(err)
		}
		got, err := ledger.ByID(context.Background(), r.OpID)
		if err != nil || got.Reconciles != 1 || strings.Contains(got.LastError, writeRemoteToken) || !strings.Contains(got.LastError, "[redacted]") {
			t.Fatalf("listing with a failing body: op %+v %v", got, err)
		}
	})
}

// retryAfterCases are server waits longer than the old one-hour clamp and the
// adapter's old 24h parse cap, as seconds and as an HTTP date (rendered
// against the clock the protocol and the adapter share).
var retryAfterCases = []struct {
	name   string
	header func(now time.Time) string
	wait   time.Duration
}{
	{"7200 seconds", func(time.Time) string { return "7200" }, 2 * time.Hour},
	{"two days in seconds", func(time.Time) string { return "172800" }, 48 * time.Hour},
	{"http date two hours on", func(now time.Time) string { return now.Add(2 * time.Hour).Format(http.TimeFormat) }, 2 * time.Hour},
	{"http date fifty hours on", func(now time.Time) string { return now.Add(50 * time.Hour).Format(http.TimeFormat) }, 50 * time.Hour},
}

// A long Retry-After is persisted as the server named it, and no request is
// made before that deadline: not the POST after a 429, not the evidence
// search after a listing 429.
func TestAdapterLongRetryAfterIsKept(t *testing.T) {
	for _, tc := range retryAfterCases {
		t.Run("post "+tc.name, func(t *testing.T) {
			clock := ytwritetest.NewClock()
			var y *recordedYouTrack
			y = &recordedYouTrack{answer: func(w http.ResponseWriter, _ *http.Request) {
				if y.countOf(http.MethodPost) == 1 {
					w.Header().Set("Retry-After", tc.header(clock.Now()))
					w.WriteHeader(http.StatusTooManyRequests)
					return
				}
				_, _ = w.Write(fixture(t, "comment_create.json"))
			}}
			rm := y.serve(t)
			rm.Now = clock.Now
			ledger := ytwritetest.NewMemLedger()
			p, err := ytwrite.New(ytwrite.Config{Ledger: ledger, Remote: rm, Now: clock.Now})
			if err != nil {
				t.Fatal(err)
			}
			r, err := p.AppendComment(context.Background(), ytwrite.CommentRequest{Principal: "hk:task-comments", Key: "task-comment:3", Actor: "worker", TaskID: 77, YTIssueID: "2-77", Body: "본문"})
			if err != nil {
				t.Fatal(err)
			}
			op, _ := ledger.ByID(context.Background(), r.OpID)
			if want := clock.Now().Add(tc.wait); op.State != ytwrite.StatePrepared || !op.NextAttemptAt.Equal(want) {
				t.Fatalf("op %s next attempt %v, want prepared until %v", op.State, op.NextAttemptAt, want)
			}
			clock.Advance(tc.wait - time.Second)
			if _, err := p.Drive(context.Background(), 10); err != nil {
				t.Fatal(err)
			}
			if n := y.countOf(http.MethodPost); n != 1 {
				t.Fatalf("%d POSTs one second before the server's deadline", n)
			}
			clock.Advance(time.Second)
			if _, err := p.Drive(context.Background(), 10); err != nil {
				t.Fatal(err)
			}
			if op, _ := ledger.ByID(context.Background(), r.OpID); op.State != ytwrite.StateApplied || y.countOf(http.MethodPost) != 2 {
				t.Fatalf("at the deadline: op %s, %d POSTs", op.State, y.countOf(http.MethodPost))
			}
		})
		t.Run("listing "+tc.name, func(t *testing.T) {
			clock := ytwritetest.NewClock()
			var y *recordedYouTrack
			y = &recordedYouTrack{answer: func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodPost:
					w.WriteHeader(http.StatusBadGateway)
				case y.countOf(http.MethodGet) == 1:
					w.Header().Set("Retry-After", tc.header(clock.Now()))
					w.WriteHeader(http.StatusTooManyRequests)
				default:
					_, _ = w.Write([]byte(`[]`))
				}
			}}
			rm := y.serve(t)
			rm.Now = clock.Now
			ledger := ytwritetest.NewMemLedger()
			p, err := ytwrite.New(ytwrite.Config{Ledger: ledger, Remote: rm, Now: clock.Now})
			if err != nil {
				t.Fatal(err)
			}
			r, err := p.AppendComment(context.Background(), ytwrite.CommentRequest{Principal: "hk:task-comments", Key: "task-comment:4", Actor: "worker", TaskID: 77, YTIssueID: "2-77", Body: "본문"})
			if err != nil || r.State != ytwrite.StateRemoteUnknown {
				t.Fatalf("%+v %v", r, err)
			}
			clock.Advance(time.Minute)
			if _, err := p.Drive(context.Background(), 10); err != nil {
				t.Fatal(err)
			}
			op, _ := ledger.ByID(context.Background(), r.OpID)
			if want := clock.Now().Add(tc.wait); op.State != ytwrite.StateRemoteUnknown || !op.NextAttemptAt.Equal(want) || y.countOf(http.MethodGet) != 1 {
				t.Fatalf("op %s next search %v after %d GETs, want remote_unknown until %v", op.State, op.NextAttemptAt, y.countOf(http.MethodGet), want)
			}
			clock.Advance(tc.wait - time.Second)
			if _, err := p.Drive(context.Background(), 10); err != nil {
				t.Fatal(err)
			}
			if n := y.countOf(http.MethodGet); n != 1 {
				t.Fatalf("%d GETs one second before the server's deadline", n)
			}
			clock.Advance(time.Second)
			if _, err := p.Drive(context.Background(), 10); err != nil {
				t.Fatal(err)
			}
			if y.countOf(http.MethodGet) < 2 || y.countOf(http.MethodPost) != 1 {
				t.Fatalf("at the deadline: %d GETs, %d POSTs; want the search and no second POST", y.countOf(http.MethodGet), y.countOf(http.MethodPost))
			}
		})
	}
}

// Retry-After is capped only past 30 days, a sanity bound for absurd values:
// 30 days is kept to the second, anything longer is recorded as 30 days.
func TestRetryAfterSanityCapBoundary(t *testing.T) {
	const capWait = 30 * 24 * time.Hour
	for _, tc := range []struct {
		name   string
		header func(now time.Time) string
		want   time.Duration
	}{
		{"30 days in seconds", func(time.Time) string { return "2592000" }, capWait},
		{"30 days and a second", func(time.Time) string { return "2592001" }, capWait},
		{"http date 31 days on", func(now time.Time) string { return now.Add(31 * 24 * time.Hour).Format(http.TimeFormat) }, capWait},
		{"29 days in seconds", func(time.Time) string { return "2505600" }, 29 * 24 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := ytwritetest.NewClock()
			y := &recordedYouTrack{answer: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Retry-After", tc.header(clock.Now()))
				w.WriteHeader(http.StatusTooManyRequests)
			}}
			rm := y.serve(t)
			rm.Now = clock.Now
			ledger := ytwritetest.NewMemLedger()
			p, err := ytwrite.New(ytwrite.Config{Ledger: ledger, Remote: rm, Now: clock.Now})
			if err != nil {
				t.Fatal(err)
			}
			r, err := p.AppendComment(context.Background(), ytwrite.CommentRequest{Principal: "hk:task-comments", Key: "task-comment:5", Actor: "worker", TaskID: 77, YTIssueID: "2-77", Body: "본문"})
			if err != nil {
				t.Fatal(err)
			}
			op, _ := ledger.ByID(context.Background(), r.OpID)
			if want := clock.Now().Add(tc.want); !op.NextAttemptAt.Equal(want) {
				t.Fatalf("next attempt %v, want %v", op.NextAttemptAt, want)
			}
		})
	}
}

func TestAdapterEnumeratesActualRevisionAndDeletion(t *testing.T) {
	var page []json.RawMessage
	if err := json.Unmarshal(fixture(t, "comments_page.json"), &page); err != nil {
		t.Fatal(err)
	}
	y := &recordedYouTrack{answer: func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.Method != http.MethodGet || r.URL.Path != "/api/issues/2-77/comments" {
			t.Errorf("request %s %s", r.Method, r.URL.Path)
		}
		for _, f := range []string{"id", "text", "created", "updated", "deleted", "author(id,login,name)"} {
			if !strings.Contains(q.Get("fields"), f) {
				t.Errorf("fields %q lacks %s", q.Get("fields"), f)
			}
		}
		skip, _ := strconv.Atoi(q.Get("$skip"))
		top, _ := strconv.Atoi(q.Get("$top"))
		end := min(len(page), skip+top)
		out := []json.RawMessage{}
		if skip < len(page) {
			out = page[skip:end]
		}
		_ = json.NewEncoder(w).Encode(out)
	}}
	got, err := y.serve(t).ListComments(context.Background(), "2-77", 2)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]ytwrite.RemoteComment{}
	for _, c := range got {
		byID[c.ID] = c
	}
	if len(byID) != 3 {
		t.Fatalf("comments = %+v", got)
	}
	hk := byID["4-102"]
	if !strings.HasSuffix(hk.Text, "\n\n<!-- hk-op:17 -->") || !hk.UpdatedAt.Equal(time.UnixMilli(1731153600000)) || hk.Deleted {
		t.Fatalf("hk comment = %+v", hk)
	}
	if imported := byID["4-101"]; !imported.UpdatedAt.Equal(time.UnixMilli(1731117600000)) {
		t.Fatalf("a never-updated comment takes its created time: %+v", imported)
	}
	if !byID["4-103"].Deleted {
		t.Fatalf("deleted flag lost: %+v", byID["4-103"])
	}
	// The author's id, for the comment audit; "" when the listing has none.
	if hk.AuthorID != "1-1" || byID["4-101"].AuthorID != "1-7" || byID["4-103"].AuthorID != "" {
		t.Fatalf("author ids %q %q %q", hk.AuthorID, byID["4-101"].AuthorID, byID["4-103"].AuthorID)
	}
	// The created time apart from the revision, for the rebuild drill.
	if !byID["4-103"].CreatedAt.Equal(time.UnixMilli(1731150000000)) || !byID["4-101"].CreatedAt.Equal(time.UnixMilli(1731117600000)) {
		t.Fatalf("created times %v %v", byID["4-103"].CreatedAt, byID["4-101"].CreatedAt)
	}
}

func TestAdapterGetIssueDecodesRecordedIssue(t *testing.T) {
	y := &recordedYouTrack{answer: func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/issues/HK-7" {
			_, _ = w.Write(fixture(t, "issue_hk7.json"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write(fixture(t, "error_404.json"))
	}}
	rm := y.serve(t)
	issue, err := rm.GetIssue(context.Background(), "HK-7")
	if err != nil || issue.ID != "2-77" || issue.IDReadable != "HK-7" || !strings.HasPrefix(issue.Summary, "#7 ") || issue.Project == nil || issue.Project.ID != "0-2" {
		t.Fatalf("issue = %+v, %v", issue, err)
	}
	if _, err := rm.GetIssue(context.Background(), "HK-404"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing issue: %v", err)
	}
}

// A reply that does not decode must not echo its bytes into the error: the
// op log keeps that text. encoding/json quotes the offending character.
func TestAdapterDecodeErrorDoesNotEchoReplyBody(t *testing.T) {
	y := &recordedYouTrack{answer: func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("#token-SECRET not json"))
	}}
	rm := y.serve(t)
	_, err := rm.GetIssue(context.Background(), "HK-7")
	if err == nil {
		t.Fatal("a body that is not JSON decoded")
	}
	if strings.Contains(err.Error(), "token-SECRET") || strings.Contains(err.Error(), "'#'") {
		t.Fatalf("decode error echoes the reply body: %q", err.Error())
	}
}
