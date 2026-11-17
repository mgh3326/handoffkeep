package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

func (s *Store) listDeployRecordsByPrefixMySQL(ctx context.Context, prefixes []string, limit int) (map[string]DeployRecordPage, error) {
	out := make(map[string]DeployRecordPage, len(prefixes))
	if len(prefixes) == 0 {
		return out, nil
	}
	if limit < 1 {
		limit = 100
	}
	if limit > maxDeployRecordsPerPrefix {
		limit = maxDeployRecordsPerPrefix
	}
	for _, prefix := range prefixes {
		if prefix == "" || !validText(prefix, maxDocumentPrefixBytes) {
			return nil, errors.New("invalid deploy record prefix")
		}
		if _, exists := out[prefix]; exists {
			continue
		}
		page := DeployRecordPage{Records: []StoredDeployRecord{}}
		rows, err := s.db().Query(ctx, `SELECT `+deployServiceProjectionColumns+` FROM deploy_record_projection WHERE `+
			`LEFT(`+"`key`"+`,CHAR_LENGTH(?))=? ORDER BY `+"`key`"+` DESC LIMIT ?`, prefix, prefix, limit+1)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var x StoredDeployRecord
			var raw []byte
			if err = rows.Scan(&x.Key, &x.SHA256, &raw, &x.Decoded); err != nil {
				rows.Close()
				return nil, err
			}
			if len(page.Records) == limit {
				page.More = true
				continue
			}
			if err = json.Unmarshal(raw, &x.Record); err != nil {
				rows.Close()
				return nil, err
			}
			if keyAt, ok := DeployRecordKeyTime(x.Key); ok {
				x.CreatedAt = keyAt
			}
			page.Records = append(page.Records, x)
		}
		if err = rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
		out[prefix] = page
	}
	return out, nil
}

const deployServiceProjectionColumns = "`key`,sha256,record,decoded"

func deployRecordForPRMySQL(ctx context.Context, tx transaction, pr string) (StoredDeployRecord, bool, error) {
	rows, err := tx.Query(ctx, `SELECT `+deployServiceProjectionColumns+` FROM deploy_record_projection
		WHERE `+"`key`"+` LIKE 'deploy/%' AND decoded=TRUE AND JSON_CONTAINS(CAST(record AS JSON),JSON_QUOTE(?),'$.included_prs')
		ORDER BY `+"`key`"+` DESC`, pr)
	if err != nil {
		return StoredDeployRecord{}, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var x StoredDeployRecord
		var raw []byte
		if err = rows.Scan(&x.Key, &x.SHA256, &raw, &x.Decoded); err != nil {
			return StoredDeployRecord{}, false, err
		}
		if err = json.Unmarshal(raw, &x.Record); err != nil {
			return StoredDeployRecord{}, false, fmt.Errorf("decode deploy record projection: %w", err)
		}
		if keyAt, ok := DeployRecordKeyTime(x.Key); ok {
			x.CreatedAt = keyAt
		}
		prs, ok := deployedPRs(x)
		if !ok {
			continue
		}
		for _, listed := range prs {
			if listed == pr {
				return x, true, nil
			}
		}
	}
	if err = rows.Err(); err != nil {
		return StoredDeployRecord{}, false, err
	}
	return StoredDeployRecord{}, false, nil
}
