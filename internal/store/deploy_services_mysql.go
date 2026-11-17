package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

func scanDeployServiceMySQL(r row) (DeployService, error) {
	var x DeployService
	var prefixes []byte
	err := r.Scan(&x.Name, &x.Repo, &prefixes, &x.Active, &x.CreatedAt, &x.UpdatedAt)
	if err != nil {
		return x, err
	}
	err = json.Unmarshal(prefixes, &x.RecordPrefixes)
	if x.RecordPrefixes == nil {
		x.RecordPrefixes = []string{}
	}
	return x, err
}

func listDeployServicesMySQL(ctx context.Context, q rowsQuerier) ([]DeployService, error) {
	rs, err := q.Query(ctx, `SELECT `+deployServiceColumns+` FROM deploy_services ORDER BY name`)
	if err != nil {
		return nil, err
	}
	return collectRows(rs, scanDeployServiceMySQL)
}

func (s *Store) putDeployServiceMySQL(ctx context.Context, x DeployService) (DeployService, error) {
	if err := checkDeployService(x); err != nil {
		return DeployService{}, err
	}
	prefixes, err := json.Marshal(x.RecordPrefixes)
	if err != nil {
		return DeployService{}, err
	}
	tx, release, err := s.begin(ctx)
	if err != nil {
		return DeployService{}, err
	}
	defer release()
	defer tx.Rollback(ctx)
	if _, err = mysqlTransactionLock(ctx, tx, mysqlLockKey("text", "deploy_services_prefixes"), false); err != nil {
		return DeployService{}, err
	}
	var conflict bool
	err = tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM deploy_services s
		JOIN JSON_TABLE(s.record_prefixes,'$[*]' COLUMNS(existing_prefix VARCHAR(768) PATH '$')) e
		JOIN JSON_TABLE(?,'$[*]' COLUMNS(proposed_prefix VARCHAR(768) PATH '$')) p
		WHERE s.active AND s.name<>? AND (LEFT(e.existing_prefix,CHAR_LENGTH(p.proposed_prefix))=p.proposed_prefix OR LEFT(p.proposed_prefix,CHAR_LENGTH(e.existing_prefix))=e.existing_prefix)
	)`, prefixes, x.Name).Scan(&conflict)
	if err != nil {
		return DeployService{}, err
	}
	if conflict {
		return DeployService{}, fmt.Errorf("%w: record prefix overlaps another active service", ErrInvalidDeployService)
	}
	now := time.Now().UTC()
	_, err = tx.Exec(ctx, `INSERT INTO deploy_services(name,repo,record_prefixes,active,created_at,updated_at) VALUES(?,?,?,TRUE,?,?)
		ON DUPLICATE KEY UPDATE repo=VALUES(repo),record_prefixes=VALUES(record_prefixes),active=TRUE,updated_at=VALUES(updated_at)`, x.Name, x.Repo, prefixes, now, now)
	if err != nil {
		return DeployService{}, err
	}
	out, err := scanDeployServiceMySQL(tx.QueryRow(ctx, `SELECT `+deployServiceColumns+` FROM deploy_services WHERE name=?`, x.Name))
	if err != nil {
		return DeployService{}, err
	}
	return out, tx.Commit(ctx)
}

func (s *Store) deactivateDeployServiceMySQL(ctx context.Context, name string) (DeployService, error) {
	if !validName(name) {
		return DeployService{}, fmt.Errorf("%w: name must match [A-Za-z0-9._-]{1,128}", ErrInvalidDeployService)
	}
	tx, release, err := s.begin(ctx)
	if err != nil {
		return DeployService{}, err
	}
	defer release()
	defer tx.Rollback(ctx)
	_, err = tx.Exec(ctx, `UPDATE deploy_services SET active=FALSE,updated_at=? WHERE name=?`, time.Now().UTC(), name)
	if err != nil {
		return DeployService{}, err
	}
	out, err := scanDeployServiceMySQL(tx.QueryRow(ctx, `SELECT `+deployServiceColumns+` FROM deploy_services WHERE name=?`, name))
	if errors.Is(err, ErrNoRows) {
		return DeployService{}, ErrServiceNotFound
	}
	if err != nil {
		return DeployService{}, err
	}
	return out, tx.Commit(ctx)
}
