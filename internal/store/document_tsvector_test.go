package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// The mapping needs no database: only the driver's error code and message.
func TestDocumentBodyWriteErrorMapsOnlyTheTsvectorLimit(t *testing.T) {
	tsvector := fmt.Errorf("insert: %w", &pgconn.PgError{Code: "54000", Message: "string is too long for tsvector (1100000 bytes, max 1048575 bytes)"})
	if err := documentBodyWriteError(tsvector); !errors.Is(err, ErrDocumentTooLarge) {
		t.Fatalf("tsvector refusal: err=%v, want ErrDocumentTooLarge", err)
	}
	other54000 := &pgconn.PgError{Code: "54000", Message: "index row size exceeds maximum"}
	if err := documentBodyWriteError(other54000); errors.Is(err, ErrDocumentTooLarge) || err != other54000 {
		t.Fatalf("other program limit: err=%v, want it passed through", err)
	}
	if err := documentBodyWriteError(nil); err != nil {
		t.Fatalf("nil: err=%v", err)
	}
}

// distinctTokenBody fills up to n bytes with one 64-character hex digest per
// line, each digest of a different integer, so every line is a new lexeme for
// the full-text index. It never exceeds n bytes.
func distinctTokenBody(n int) string {
	var b strings.Builder
	for i := 0; b.Len()+65 <= n; i++ {
		sum := sha256.Sum256([]byte(strconv.Itoa(i)))
		b.WriteString(hex.EncodeToString(sum[:]))
		b.WriteByte('\n')
	}
	return b.String()
}

// A body under DocumentMaxBytes can still overflow the PostgreSQL tsvector of
// documents_fts (about 1 MiB of lexemes). The store must refuse it with
// ErrDocumentTooLarge and store nothing, while a repetitive body of the same
// length still stores. MySQL has no tsvector limit, so the test is skipped there.
func TestPutDocumentRefusesTooManyDistinctWordsForTheIndex(t *testing.T) {
	s := schemaStore(t, "document_tsvector_limit")
	if s.dialect == mysqlDialect {
		t.Skip("MySQL full-text indexing has no tsvector limit")
	}
	ctx := t.Context()

	distinct := Document{Key: "tsvector/distinct", Kind: "note", CreatedBy: "tester", Body: distinctTokenBody(DocumentMaxBytes)}
	_, _, err := s.PutDocument(ctx, distinct)
	if !errors.Is(err, ErrDocumentTooLarge) {
		t.Fatalf("err=%v, want ErrDocumentTooLarge", err)
	}
	if !strings.Contains(err.Error(), "body has too many distinct words to index") {
		t.Fatalf("reason %q does not say why the body was refused", err.Error())
	}
	if _, found, err := s.GetDocument(ctx, distinct.Key); err != nil || found {
		t.Fatalf("refused document stored: found=%v err=%v", found, err)
	}

	repetitive := Document{Key: "tsvector/repetitive", Kind: "note", CreatedBy: "tester", Body: strings.Repeat("x", DocumentMaxBytes)}
	if _, changed, err := s.PutDocument(ctx, repetitive); err != nil || !changed {
		t.Fatalf("repetitive body of the same length: changed=%v err=%v", changed, err)
	}
}
