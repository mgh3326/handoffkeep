package store

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/mgh3326/handoffkeep/internal/wrkrecords"
)

type wrkPrincipalKey struct{}
type wrkPrincipal struct {
	Actor   string
	Receipt *wrkrecords.Receipt
}

// WithWrkPrincipal is called only with a transport-authenticated client id.
// UI/local calls intentionally have no implicit central-role principal.
func WithWrkPrincipal(ctx context.Context, actor string, receipt *wrkrecords.Receipt) context.Context {
	return context.WithValue(ctx, wrkPrincipalKey{}, wrkPrincipal{actor, receipt})
}
func WrkPrincipal(ctx context.Context) (string, *wrkrecords.Receipt) {
	p, _ := ctx.Value(wrkPrincipalKey{}).(wrkPrincipal)
	return p.Actor, p.Receipt
}
func (s *Store) wrkFence(ctx context.Context, q wrkQuerier, in wrkrecords.LegacyIntent) error {
	snapshot, e := loadWrkSnapshot(ctx, q)
	if e != nil {
		return e
	}
	actor, receipt := WrkPrincipal(ctx)
	if in.Receipt == nil {
		in.Receipt = receipt
	}
	return snapshot.CheckLegacy(s.WrkEngine().Policy, actor, in)
}

// WrkFence is the service precheck. Every write repeats it inside its own
// transaction after acquiring the row lock; this precheck cannot authorize.
func (s *Store) WrkFence(ctx context.Context, in wrkrecords.LegacyIntent) error {
	return s.wrkFence(ctx, s.pool, in)
}
func (s *Store) wrkTaskFenceTx(ctx context.Context, tx pgx.Tx, x Task, to string, binding bool) error {
	return s.wrkFence(ctx, tx, wrkrecords.LegacyIntent{TaskID: x.ID, CurrentJob: x.Refs.JobID, To: to, Binding: binding})
}

func wrkTablesExist(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}) bool {
	var exists bool
	return q.QueryRow(ctx, `SELECT to_regclass('wrk_records') IS NOT NULL`).Scan(&exists) == nil && exists
}
