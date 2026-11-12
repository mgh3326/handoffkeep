package store

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// The canonical side per content kind (hk 4122, docs/youtrack-write-protocol.md
// §Authority). Authority hk is today's behaviour exactly. While a kind is
// youtrack: the import read API (/v1/import/*) answers empty pages for it, the
// legacy `youtrack sync` and `adopt` refuse, `youtrack audit` repairs it from
// YouTrack to PG and enqueues nothing, and a write the protocol cannot send for
// it is refused instead of committed with a born-rejected op.

// YTAuthorityKind is one content kind the switch flips.
type YTAuthorityKind string

const (
	YTAuthorityTasks     YTAuthorityKind = "tasks"
	YTAuthorityComments  YTAuthorityKind = "comments"
	YTAuthorityDocuments YTAuthorityKind = "documents"
	YTAuthorityDecisions YTAuthorityKind = "decisions"
)

// YTAuthorityKinds is every kind, in the order show and set report them.
var YTAuthorityKinds = []YTAuthorityKind{YTAuthorityTasks, YTAuthorityComments, YTAuthorityDocuments, YTAuthorityDecisions}

// YTAuthoritySide is the canonical side of a kind.
type YTAuthoritySide string

const (
	// YTAuthorityHK: PG is canonical (the default, and the rollback).
	YTAuthorityHK YTAuthoritySide = "hk"
	// YTAuthorityYouTrack: YouTrack is canonical.
	YTAuthorityYouTrack YTAuthoritySide = "youtrack"
)

// YTAuthority is one kind's row. A kind never switched has no SwitchedAt.
type YTAuthority struct {
	Kind       YTAuthorityKind `json:"kind"`
	Authority  YTAuthoritySide `json:"authority"`
	SwitchedAt *time.Time      `json:"switched_at"`
	SwitchedBy string          `json:"switched_by"`
	Note       string          `json:"note"`
}

// ErrYTAuthorityRefused is a switch to youtrack whose preconditions do not
// hold: the kind's write mode is off, or the kind has unresolved ops. The
// text after "yt_authority_refused: " names the kind and the precondition.
var ErrYTAuthorityRefused = errors.New("yt_authority_refused")

// ErrYTCommentUnsendable is a comment the write protocol cannot send while
// comments are canonical in YouTrack: it is refused, not committed. The text
// after "yt_comment_unsendable: " is the protocol's refusal.
var ErrYTCommentUnsendable = errors.New("yt_comment_unsendable")

// ytAuthorityPrincipals are the op principals whose writes belong to each
// kind: the ops that must be resolved before the kind switches. Decisions have
// no op in this release.
var ytAuthorityPrincipals = map[YTAuthorityKind][]string{
	YTAuthorityTasks:     {YTIssuePrincipal, YTFieldsPrincipal},
	YTAuthorityComments:  {YTCommentPrincipal},
	YTAuthorityDocuments: {ytDocCommentPrincipal},
}

func validYTAuthorityKind(k YTAuthorityKind) bool { return slices.Contains(YTAuthorityKinds, k) }

func validYTAuthoritySide(a YTAuthoritySide) bool {
	return a == YTAuthorityHK || a == YTAuthorityYouTrack
}

const ytAuthorityColumns = `kind, authority, switched_at, switched_by, note`

func scanYTAuthority(row interface{ Scan(...any) error }, a *YTAuthority) error {
	if err := row.Scan(&a.Kind, &a.Authority, &a.SwitchedAt, &a.SwitchedBy, &a.Note); err != nil {
		return err
	}
	if a.SwitchedAt != nil {
		at := a.SwitchedAt.UTC()
		a.SwitchedAt = &at
	}
	return nil
}

// ytAuthoritiesTx reads every kind's row, in YTAuthorityKinds order; a kind
// with no row is hk. lock is "" or a row-lock clause.
func ytAuthoritiesTx(ctx context.Context, q interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}, lock string) ([]YTAuthority, error) {
	rows, err := q.Query(ctx, `SELECT `+ytAuthorityColumns+` FROM yt_authority ORDER BY kind`+lock)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byKind := map[YTAuthorityKind]YTAuthority{}
	for rows.Next() {
		var a YTAuthority
		if err := scanYTAuthority(rows, &a); err != nil {
			return nil, err
		}
		byKind[a.Kind] = a
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]YTAuthority, 0, len(YTAuthorityKinds))
	for _, k := range YTAuthorityKinds {
		a, ok := byKind[k]
		if !ok || !validYTAuthoritySide(a.Authority) {
			a = YTAuthority{Kind: k, Authority: YTAuthorityHK}
		}
		out = append(out, a)
	}
	return out, nil
}

// ytAuthorityTx reads one kind's side; no row is hk. lock is "" or a
// row-lock clause (the comment writers read FOR SHARE, so a switch waits for
// a comment in flight and the comment sees a switch that committed first).
func ytAuthorityTx(ctx context.Context, q ytRowQuerier, kind YTAuthorityKind, lock string) (YTAuthoritySide, error) {
	var side YTAuthoritySide
	err := q.QueryRow(ctx, `SELECT authority FROM yt_authority WHERE kind=$1`+lock, string(kind)).Scan(&side)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !validYTAuthoritySide(side)) {
		return YTAuthorityHK, nil
	}
	return side, err
}

// YTAuthorities reads every kind's row.
func (s *Store) YTAuthorities(ctx context.Context) ([]YTAuthority, error) {
	return ytAuthoritiesTx(ctx, s.pool, "")
}

// YTAuthorityOf reads one kind's side; an unknown kind is hk.
func (s *Store) YTAuthorityOf(ctx context.Context, kind YTAuthorityKind) (YTAuthoritySide, error) {
	return ytAuthorityTx(ctx, s.pool, kind, "")
}

// ytAuthorityWriteModeOff explains why kind's write mode in this process is
// not on, or returns "" when it is. Tasks need both the issue and the field
// copies; decisions have no write mode in this release.
func (s *Store) ytAuthorityWriteModeOff(kind YTAuthorityKind) string {
	switch kind {
	case YTAuthorityTasks:
		if m, _ := s.ytIssuesNow(); m != YTIssuesOn {
			return "HANDOFFKEEP_YT_ISSUES_MODE is not on"
		}
		if m, _ := s.ytFieldsNow(); m != YTFieldsOn {
			return "HANDOFFKEEP_YT_FIELDS_MODE is not on"
		}
		return ""
	case YTAuthorityComments:
		if s.ytCommentsModeNow() != YTCommentsOn {
			return "HANDOFFKEEP_YT_COMMENTS_MODE is not on"
		}
		return ""
	case YTAuthorityDocuments:
		if s.ytDocCommentsNow() != YTDocCommentsOn {
			return "HANDOFFKEEP_YT_DOC_COMMENTS_MODE is not on"
		}
		return ""
	default:
		return "this release has no write mode for " + string(kind)
	}
}

// YTAuthoritySetMax bounds the operator's note.
const YTAuthoritySetMax = 2000

// SetYTAuthority switches kinds to side in one transaction and returns every
// kind's row before and after. A kind already on side is left as it is
// (its switched_at, switched_by and note are kept). A switch to youtrack is
// ErrYTAuthorityRefused unless, for each kind it changes, this process's
// write mode for the kind is on and the kind has no prepared, dispatched or
// remote_unknown op; if any kind is refused, none changes. A switch to hk
// always succeeds: it is the rollback.
//
// Locks: FOR UPDATE on the yt_authority rows (in kind order), then plain reads
// of yt_write_ops. It takes no other lock (lockorder.go).
func (s *Store) SetYTAuthority(ctx context.Context, kinds []YTAuthorityKind, side YTAuthoritySide, by, note string) (before, after []YTAuthority, err error) {
	if len(kinds) == 0 || !validYTAuthoritySide(side) || strings.TrimSpace(by) == "" || !ValidActor(by) ||
		strings.TrimSpace(note) == "" || !validText(note, YTAuthoritySetMax) {
		return nil, nil, errors.New("invalid authority set: want kinds, hk or youtrack, --by and --note")
	}
	seen := map[YTAuthorityKind]bool{}
	for _, k := range kinds {
		if !validYTAuthorityKind(k) || seen[k] {
			return nil, nil, fmt.Errorf("invalid authority set: unknown or repeated kind %q", k)
		}
		seen[k] = true
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback(ctx)
	// A row a database lost (or never seeded) is hk; give it a row to lock.
	if _, err := tx.Exec(ctx, `INSERT INTO yt_authority(kind) SELECT unnest($1::text[]) ON CONFLICT DO NOTHING`, ytAuthorityKindStrings(YTAuthorityKinds)); err != nil {
		return nil, nil, err
	}
	before, err = ytAuthoritiesTx(ctx, tx, " FOR UPDATE")
	if err != nil {
		return nil, nil, err
	}
	var change []YTAuthorityKind
	for _, a := range before {
		if seen[a.Kind] && a.Authority != side {
			change = append(change, a.Kind)
		}
	}
	if side == YTAuthorityYouTrack {
		var refusals []string
		for _, k := range change {
			if off := s.ytAuthorityWriteModeOff(k); off != "" {
				refusals = append(refusals, string(k)+": "+off)
				continue
			}
			var pending int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM yt_write_ops WHERE principal = ANY($1)
				AND state IN ('prepared','dispatched','remote_unknown')`, ytAuthorityPrincipals[k]).Scan(&pending); err != nil {
				return nil, nil, err
			}
			if pending > 0 {
				refusals = append(refusals, fmt.Sprintf("%s: %d ops are prepared, dispatched or remote_unknown (youtrack ops)", k, pending))
			}
		}
		if len(refusals) > 0 {
			return nil, nil, fmt.Errorf("%w: %s", ErrYTAuthorityRefused, strings.Join(refusals, "; "))
		}
	}
	if len(change) > 0 {
		if _, err := tx.Exec(ctx, `UPDATE yt_authority SET authority=$2, switched_at=$3, switched_by=$4, note=$5 WHERE kind = ANY($1)`,
			ytAuthorityKindStrings(change), string(side), time.Now().UTC(), by, note); err != nil {
			return nil, nil, err
		}
	}
	after, err = ytAuthoritiesTx(ctx, tx, "")
	if err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, err
	}
	for _, k := range change {
		log.Printf("ytwrite.authority_set kind=%s authority=%s switched_by=%q note=%q", k, side, by, note)
	}
	return before, after, nil
}

func ytAuthorityKindStrings(kinds []YTAuthorityKind) []string {
	out := make([]string, len(kinds))
	for i, k := range kinds {
		out[i] = string(k)
	}
	return out
}
