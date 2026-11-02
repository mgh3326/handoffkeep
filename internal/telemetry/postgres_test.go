package telemetry

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgproto3"
)

// protocolConn exercises the real pgx query hooks over an in-memory pipe.
// There is no database server, listener, subprocess, or database dependency.
func protocolConn(t *testing.T) *pgx.Conn {
	t.Helper()
	client, peer := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer peer.Close()
		backend := pgproto3.NewBackend(peer, peer)
		if _, err := backend.ReceiveStartupMessage(); err != nil {
			return
		}
		backend.Send(&pgproto3.AuthenticationOk{})
		backend.Send(&pgproto3.ParameterStatus{Name: "server_version", Value: "17.0"})
		backend.Send(&pgproto3.ParameterStatus{Name: "standard_conforming_strings", Value: "on"})
		backend.Send(&pgproto3.ParameterStatus{Name: "client_encoding", Value: "UTF8"})
		backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
		if backend.Flush() != nil {
			return
		}
		for {
			message, err := backend.Receive()
			if err != nil {
				return
			}
			switch message.(type) {
			case *pgproto3.Query:
				backend.Send(&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")})
				backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
				if backend.Flush() != nil {
					return
				}
			case *pgproto3.Terminate:
				return
			}
		}
	}()
	t.Cleanup(func() { client.Close(); peer.Close(); <-done })
	config, err := pgx.ParseConfig("")
	if err != nil {
		t.Fatal(err)
	}
	config.TLSConfig, config.Fallbacks = nil, nil
	config.DialFunc = func(context.Context, string, string) (net.Conn, error) { return client, nil }
	config.LookupFunc = func(context.Context, string) ([]string, error) { return []string{net.IPv4(127, 0, 0, 1).String()}, nil }
	config.Host = t.Name()
	config.Tracer = QueryTracer{}
	config.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

func TestNormalizeSQL(t *testing.T) {
	for _, test := range []struct{ sql, want string }{
		{"SELECT $1, $20 FROM tasks WHERE id=123", "SELECT ?, ? FROM tasks WHERE id=?"},
		{"SELECT 'private '' document', E'private\\' content', U&'private', B'010', X'FF'", "SELECT ?, E?, U&?, B?, X?"},
		{"SELECT $$private document$$, $tag_1$private$tag_1$", "SELECT ?, ?"},
		{"SELECT 42, -1.5e+20, .2, 0xFF, 0o77, 0b11, column1", "SELECT ?, -?, ?, ?, ?, ?, column1"},
		{"SELECT /* private /* nested */ content */ 1 -- private\n FROM tasks", "SELECT ? FROM tasks"},
		{"SELECT \"private\"\"identifier\" FROM tasks", "SELECT ? FROM tasks"},
		{"SELECT 'unterminated private", "[filtered statement]"},
		{"SELECT $tag$unterminated private", "[filtered statement]"},
		{"SELECT /*unterminated private", "[filtered statement]"},
		{"SELECT " + strings.Repeat("x", 16<<10), "[filtered statement]"},
	} {
		t.Run(test.want, func(t *testing.T) {
			got := normalizeSQL(test.sql)
			if got != test.want {
				t.Fatalf("got %q want %q", got, test.want)
			}
			if twice := normalizeSQL(got); twice != got {
				t.Fatalf("normalization not idempotent: %q -> %q", got, twice)
			}
		})
	}
}
