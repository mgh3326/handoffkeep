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
		lo, hi := keyPrefixBounds(prefix)
		rows, err := s.db().Query(ctx, `SELECT d.`+"`key`"+`,d.sha256,d.created_at,p.record,p.decoded,`+deployRecordBodyFallbackMySQL+
			` FROM documents d LEFT JOIN deploy_record_projection p ON p.`+"`key`"+`=d.`+"`key`"+` AND p.sha256=d.sha256
			WHERE d.`+"`key`"+`>=? AND d.`+"`key`"+`<? ORDER BY d.`+"`key`"+` DESC LIMIT ?`, lo, hi, limit+1)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var x StoredDeployRecord
			var raw []byte
			var decoded *bool
			var body *string
			if err = rows.Scan(&x.Key, &x.SHA256, &x.CreatedAt, &raw, &decoded, &body); err != nil {
				rows.Close()
				return nil, err
			}
			if len(page.Records) == limit {
				page.More = true
				continue
			}
			if body != nil {
				x = parseStoredDeployRecord(x.Key, x.SHA256, *body, x.CreatedAt)
			} else {
				if err = json.Unmarshal(raw, &x.Record); err != nil {
					rows.Close()
					return nil, err
				}
				x.Decoded = *decoded
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

const deployRecordBodyFallbackMySQL = "CASE WHEN p.`key` IS NULL OR NOT p.decoded\n\tOR NOT JSON_CONTAINS_PATH(p.record,'one','$.failed_step_name') THEN d.body ELSE NULL END"

func deployRecordForPRMySQL(ctx context.Context, tx transaction, pr string) (StoredDeployRecord, bool, error) {
	lo, hi := keyPrefixBounds("deploy/")
	rows, err := tx.Query(ctx, `SELECT d.`+"`key`"+`,d.sha256,d.created_at,p.record,p.decoded,`+deployRecordBodyFallbackMySQL+
		` FROM documents d LEFT JOIN deploy_record_projection p ON p.`+"`key`"+`=d.`+"`key`"+` AND p.sha256=d.sha256
		WHERE d.`+"`key`"+`>=? AND d.`+"`key`"+`<? AND (LOCATE(?,d.body)>0 OR (p.decoded=TRUE AND JSON_CONTAINS(CAST(p.record AS JSON),JSON_QUOTE(?),'$.included_prs')))
		ORDER BY d.`+"`key`"+` DESC`, lo, hi, pr, pr)
	if err != nil {
		return StoredDeployRecord{}, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var x StoredDeployRecord
		var raw []byte
		var decoded *bool
		var body *string
		if err = rows.Scan(&x.Key, &x.SHA256, &x.CreatedAt, &raw, &decoded, &body); err != nil {
			return StoredDeployRecord{}, false, err
		}
		if body != nil {
			x = parseStoredDeployRecord(x.Key, x.SHA256, *body, x.CreatedAt)
		} else {
			if err = json.Unmarshal(raw, &x.Record); err != nil {
				return StoredDeployRecord{}, false, fmt.Errorf("decode deploy record projection: %w", err)
			}
			x.Decoded = *decoded
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
