package store

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

func (s *Store) countMergedChangesSinceMySQL(ctx context.Context, repo string, since time.Time) (int, error) {
	var count int
	var err error
	if since.IsZero() {
		err = s.db().QueryRow(ctx, `SELECT COUNT(*) FROM changes WHERE repo=? AND state='merged'`, repo).Scan(&count)
	} else {
		err = s.db().QueryRow(ctx, `SELECT COUNT(*) FROM changes WHERE repo=? AND state='merged' AND merged_at>?`, repo, since.UTC()).Scan(&count)
	}
	return count, err
}

// lagStatusMySQL is deployLagStatusTx for MySQL: the same measure, `?` text.
func lagStatusMySQL(ctx context.Context, q rowQuerier, svc DeployService, since time.Time, hasSince bool) (DeployLagServiceStatus, error) {
	st := DeployLagServiceStatus{Service: svc.Name, Repo: svc.Repo, HasDeployed: hasSince}
	if !hasSince {
		return st, nil
	}
	at := since
	st.DeployedAt = &at
	var count int
	if err := q.QueryRow(ctx, `SELECT COUNT(*) FROM changes WHERE repo=? AND state='merged' AND merged_at>?`, svc.Repo, since.UTC()).Scan(&count); err != nil {
		return st, err
	}
	st.LagCount = count
	st.Lagging = IsDeployLagging(count)
	return st, nil
}

func (s *Store) deployLagStatusesMySQL(ctx context.Context, svcs []DeployService) (map[string]DeployLagServiceStatus, error) {
	out := make(map[string]DeployLagServiceStatus, len(svcs))
	if len(svcs) == 0 {
		return out, nil
	}
	tx, release, err := s.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	defer tx.Rollback(ctx)
	lastDeploys, err := readLastSuccessfulDeploysMySQL(ctx, tx)
	if err != nil {
		return nil, err
	}
	for _, svc := range svcs {
		if _, dup := out[svc.Name]; dup {
			continue
		}
		since, ok := lastDeploys[svc.Name]
		st, err := lagStatusMySQL(ctx, tx, svc, since, ok)
		if err != nil {
			return nil, err
		}
		out[svc.Name] = st
	}
	return out, tx.Commit(ctx)
}

func (s *Store) enqueueDeployLagNoticesMySQL(ctx context.Context, now time.Time) (int, error) {
	now = now.UTC()
	tx, release, err := s.begin(ctx)
	if err != nil {
		return 0, err
	}
	defer release()
	defer tx.Rollback(ctx)
	if _, err = mysqlTransactionLock(ctx, tx, mysqlLockKey("text", "deploy_lag_notices"), false); err != nil {
		return 0, err
	}
	services, err := listDeployServicesMySQL(ctx, tx)
	if err != nil {
		return 0, err
	}
	lastDeploys, err := readLastSuccessfulDeploysMySQL(ctx, tx)
	if err != nil {
		return 0, err
	}
	enqueued := 0
	for _, svc := range services {
		if !svc.Active {
			continue
		}
		since, ok := lastDeploys[svc.Name]
		st, err := lagStatusMySQL(ctx, tx, svc, since, ok)
		if err != nil {
			return 0, err
		}
		if !st.Lagging {
			continue
		}
		inserted, err := insertNotificationOutboxOnce(ctx, tx, OutboxKindDeployLag, "operator-desk", DeployLagNoticeEventID(svc.Name, now), DeployLagNoticeText(svc.Name, st.LagCount), now)
		if err != nil {
			return 0, err
		}
		if inserted {
			enqueued++
		}
	}
	return enqueued, tx.Commit(ctx)
}

// readLastSuccessfulDeploysMySQL is readProjectLastSuccessfulDeploys for
// MySQL: the newest successful deploy per active service, keyed by service
// name and by repo base name when no other service owns that name. Each prefix
// pages newest-key-first until a page holds a success or the prefix runs out.
func readLastSuccessfulDeploysMySQL(ctx context.Context, tx transaction) (map[string]time.Time, error) {
	all, err := listDeployServicesMySQL(ctx, tx)
	if err != nil {
		return nil, err
	}
	var services []DeployService
	serviceNames := map[string]bool{}
	for _, svc := range all {
		if svc.Active {
			services = append(services, svc)
			serviceNames[svc.Name] = true
		}
	}
	result := map[string]time.Time{}
	byPrefix := map[string]*time.Time{}
	for _, svc := range services {
		for _, prefix := range svc.RecordPrefixes {
			if _, done := byPrefix[prefix]; done {
				continue
			}
			alias := strings.TrimSuffix(prefix, "/")
			if i := strings.LastIndex(alias, "/"); i >= 0 {
				alias = alias[i+1:]
			}
			names := []string{svc.Name}
			if alias != svc.Name {
				names = append(names, alias)
			}
			newest, err := lastSuccessUnderPrefixMySQL(ctx, tx, prefix, names)
			if err != nil {
				return nil, err
			}
			byPrefix[prefix] = newest
		}
	}
	for _, svc := range services {
		var newest *time.Time
		for _, prefix := range svc.RecordPrefixes {
			if t := byPrefix[prefix]; t != nil && (newest == nil || t.After(*newest)) {
				newest = t
			}
		}
		if newest == nil {
			continue
		}
		result[svc.Name] = *newest
		if idx := strings.LastIndex(svc.Repo, "/"); idx >= 0 && idx+1 < len(svc.Repo) {
			if base := svc.Repo[idx+1:]; !serviceNames[base] {
				result[base] = *newest
			}
		}
	}
	return result, nil
}

func lastSuccessUnderPrefixMySQL(ctx context.Context, tx transaction, prefix string, names []string) (*time.Time, error) {
	lo, hi := keyPrefixBounds(prefix)
	after := ""
	for {
		rs, err := tx.Query(ctx, `SELECT d.`+"`key`"+`,d.sha256,d.created_at,p.record,p.decoded,`+deployRecordBodyFallbackMySQL+
			` FROM documents d LEFT JOIN deploy_record_projection p ON p.`+"`key`"+`=d.`+"`key`"+` AND p.sha256=d.sha256
			WHERE d.`+"`key`"+`>=? AND d.`+"`key`"+`<? AND (?='' OR d.`+"`key`"+`<?) ORDER BY d.`+"`key`"+` DESC LIMIT ?`,
			lo, hi, after, after, lastSuccessPageSize)
		if err != nil {
			return nil, err
		}
		var docs []StoredDeployRecord
		for rs.Next() {
			var doc StoredDeployRecord
			var raw []byte
			var decoded *bool
			var body *string
			if err = rs.Scan(&doc.Key, &doc.SHA256, &doc.CreatedAt, &raw, &decoded, &body); err != nil {
				rs.Close()
				return nil, err
			}
			if body != nil {
				doc = parseStoredDeployRecord(doc.Key, doc.SHA256, *body, doc.CreatedAt)
			} else if raw != nil && decoded != nil && json.Unmarshal(raw, &doc.Record) == nil {
				doc.Decoded = *decoded
			}
			docs = append(docs, doc)
		}
		err = rs.Err()
		rs.Close()
		if err != nil {
			return nil, err
		}
		if newest := newestSuccessDeploy(docs, names); newest != nil {
			return newest, nil
		}
		if len(docs) < lastSuccessPageSize {
			return nil, nil
		}
		after = docs[len(docs)-1].Key
	}
}
