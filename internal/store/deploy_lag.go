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

// DeployLagStatus returns the lag of one active deploy service, measured the
// same way EnqueueDeployLagNotices measures it. The console calls this so the
// banner and the operator-desk notice cannot disagree.
func (s *Store) DeployLagStatus(ctx context.Context, svc DeployService) (DeployLagServiceStatus, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return DeployLagServiceStatus{}, err
	}
	defer tx.Rollback(ctx)
	lastDeploys, err := readProjectLastSuccessfulDeploysTx(ctx, tx)
	if err != nil {
		return DeployLagServiceStatus{}, err
	}
	since, ok := lastDeploys[svc.Name]
	return deployLagStatusTx(ctx, tx, svc, since, ok)
}

// EnqueueDeployLagNotices checks each active deploy service for deploy lag against main.
// For any service whose lag count > 2 (lagging), it inserts one notification_outbox row
// to operator-desk with event_id "deploy-lag-<service>-<YYYY-MM-DD>" (idempotent ON CONFLICT).
// Returns the number of new notifications enqueued.
func (s *Store) EnqueueDeployLagNotices(ctx context.Context, now time.Time) (int, error) {
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

	services, err := listDeployServices(ctx, tx)
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
		st, err := deployLagStatusTx(ctx, tx, svc, since, ok)
		if err != nil {
			return 0, err
		}
		if !st.Lagging {
			continue
		}
		eventID := DeployLagNoticeEventID(svc.Name, now)
		text := DeployLagNoticeText(svc.Name, st.LagCount)
		inserted, err := insertNotificationOutboxOnce(ctx, tx, OutboxKindDeployLag, "operator-desk", eventID, text, now)
		if err != nil {
			return 0, err
		}
		if inserted {
			enqueued++
		}
	}

	return enqueued, tx.Commit(ctx)
}
