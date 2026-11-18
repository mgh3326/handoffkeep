package store

import (
	"context"
	"fmt"
	"time"
)

// DeployLagThreshold is the number of merged changes behind main after which a
// service is considered lagging. "Threshold: more than 2 -> lagging" (i.e. lag > 2).
const DeployLagThreshold = 2

// DeployLagServiceStatus represents the computed lag status for a deployed service.
type DeployLagServiceStatus struct {
	Service     string     `json:"service"`
	Repo        string     `json:"repo"`
	DeployedRef string     `json:"deployed_ref,omitempty"`
	DeployedAt  *time.Time `json:"deployed_at,omitempty"`
	LagCount    int        `json:"lag_count"`
	Lagging     bool       `json:"lagging"`
	HasDeployed bool       `json:"has_deployed"`
}

// IsDeployLagging reports whether a lag count is considered lagging.
// Threshold: more than 2 -> lagging (0, 1, 2 are not lagging; 3+ is lagging).
func IsDeployLagging(lagCount int) bool {
	return lagCount > DeployLagThreshold
}

// DeployLagNoticeEventID formats the deterministic event_id for once-a-day notification.
// Keyed by service + date: "deploy-lag-<service>-<YYYY-MM-DD>".
func DeployLagNoticeEventID(service string, at time.Time) string {
	return fmt.Sprintf("deploy-lag-%s-%s", service, at.UTC().Format("2006-01-02"))
}

// DeployLagNoticeText returns Korean copy for the notification sent to operator-desk.
func DeployLagNoticeText(service string, lagCount int) string {
	return fmt.Sprintf("배포 지연 경고: %s 서비스가 main보다 %d개 변경사항 뒤처져 있습니다.", service, lagCount)
}

// CountMergedChangesSince returns the number of merged changes for a repo whose
// merge timestamp is strictly after since (or all merged changes if since is zero).
// hk records merged changes in the changes table (repo, state='merged', merged_at).
func (s *Store) CountMergedChangesSince(ctx context.Context, repo string, since time.Time) (int, error) {
	if s != nil && s.dialect == mysqlDialect {
		return s.countMergedChangesSinceMySQL(ctx, repo, since)
	}

	var count int
	var err error
	if since.IsZero() {
		err = s.pool.QueryRow(ctx, `SELECT count(*) FROM changes WHERE repo = $1 AND state = 'merged'`, repo).Scan(&count)
	} else {
		err = s.pool.QueryRow(ctx, `SELECT count(*) FROM changes WHERE repo = $1 AND state = 'merged' AND merged_at > $2`, repo, since).Scan(&count)
	}
	return count, err
}

// deployLagStatusTx is the one lag measure the console and the notifier share.
// The boundary is the service's last successful deploy (the same value
// readProjectLastSuccessfulDeploysTx returns). A service with no successful
// deploy has no boundary: its lag is unknown, so it reports HasDeployed=false,
// LagCount=0 and Lagging=false. Counting from time zero would report the whole
// merge history as lag.
func deployLagStatusTx(ctx context.Context, q rowQuerier, svc DeployService, since time.Time, hasSince bool) (DeployLagServiceStatus, error) {
	st := DeployLagServiceStatus{Service: svc.Name, Repo: svc.Repo, HasDeployed: hasSince}
	if !hasSince {
		return st, nil
	}
	at := since
	st.DeployedAt = &at
	var count int
	if err := q.QueryRow(ctx, `SELECT count(*) FROM changes WHERE repo = $1 AND state = 'merged' AND merged_at > $2`, svc.Repo, since).Scan(&count); err != nil {
		return st, err
	}
	st.LagCount = count
	st.Lagging = IsDeployLagging(count)
	return st, nil
}

// DeployLagStatuses returns the lag of each given service, measured the same
// way EnqueueDeployLagNotices measures it, in one transaction and a bounded
// number of statements whatever the service count: the last successful deploys
// once, then one count that joins every service's boundary. Counting per
// service instead ran a query (and a document scan) per service per poll
// (HANDOFFKEEP-5, hk 4923). The console calls this so the banner and the
// operator-desk notice cannot disagree.
func (s *Store) DeployLagStatuses(ctx context.Context, svcs []DeployService) (map[string]DeployLagServiceStatus, error) {
	if s != nil && s.dialect == mysqlDialect {
		return s.deployLagStatusesMySQL(ctx, svcs)
	}

	out := make(map[string]DeployLagServiceStatus, len(svcs))
	if len(svcs) == 0 {
		return out, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	lastDeploys, err := readProjectLastSuccessfulDeploysTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	// A service with no successful deploy has no boundary: its lag is unknown,
	// so it is answered here and left out of the count. Counting from time zero
	// would report the whole merge history as lag.
	var names, repos []string
	var since []time.Time
	for _, svc := range svcs {
		if _, dup := out[svc.Name]; dup {
			continue
		}
		at, ok := lastDeploys[svc.Name]
		st := DeployLagServiceStatus{Service: svc.Name, Repo: svc.Repo, HasDeployed: ok}
		if ok {
			deployedAt := at
			st.DeployedAt = &deployedAt
			names = append(names, svc.Name)
			repos = append(repos, svc.Repo)
			since = append(since, at)
		}
		out[svc.Name] = st
	}
	if len(names) > 0 {
		rows, err := tx.Query(ctx, `SELECT w.name, count(c.id)
FROM unnest($1::text[],$2::text[],$3::timestamptz[]) AS w(name,repo,since)
LEFT JOIN changes c ON c.repo=w.repo AND c.state='merged' AND c.merged_at > w.since
GROUP BY w.name`, names, repos, since)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var name string
			var count int
			if err = rows.Scan(&name, &count); err != nil {
				return nil, err
			}
			st := out[name]
			st.LagCount = count
			st.Lagging = IsDeployLagging(count)
			out[name] = st
		}
		if err = rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, tx.Commit(ctx)
}

// EnqueueDeployLagNotices checks each active deploy service for deploy lag against main.
// For any service whose lag count > 2 (lagging), it inserts one notification_outbox row
// to operator-desk with event_id "deploy-lag-<service>-<YYYY-MM-DD>" (idempotent ON CONFLICT).
// Returns the number of new notifications enqueued.
func (s *Store) EnqueueDeployLagNotices(ctx context.Context, now time.Time) (int, error) {
	if s != nil && s.dialect == mysqlDialect {
		return s.enqueueDeployLagNoticesMySQL(ctx, now)
	}

	now = now.UTC()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	// Take transaction advisory lock to serialize passes
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('deploy_lag_notices', 0))`); err != nil {
		return 0, err
	}

	services, err := listDeployServices(ctx, pgTransaction{tx})
	if err != nil {
		return 0, err
	}

	lastDeploys, err := readProjectLastSuccessfulDeploysTx(ctx, tx)
	if err != nil {
		return 0, err
	}

	enqueued := 0
	for _, svc := range services {
		if !svc.Active {
			continue
		}
		since, ok := lastDeploys[svc.Name]
		st, err := deployLagStatusTx(ctx, pgTransaction{tx}, svc, since, ok)
		if err != nil {
			return 0, err
		}
		if !st.Lagging {
			continue
		}
		eventID := DeployLagNoticeEventID(svc.Name, now)
		text := DeployLagNoticeText(svc.Name, st.LagCount)
		inserted, err := insertNotificationOutboxOnce(ctx, pgTransaction{tx}, OutboxKindDeployLag, "operator-desk", eventID, text, now)
		if err != nil {
			return 0, err
		}
		if inserted {
			enqueued++
		}
	}

	return enqueued, tx.Commit(ctx)
}
