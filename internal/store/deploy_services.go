package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var ErrInvalidDeployService = errors.New("invalid_deploy_service")

type DeployService struct {
	Name           string    `json:"name"`
	Repo           string    `json:"repo"`
	RecordPrefixes []string  `json:"record_prefixes"`
	Active         bool      `json:"active"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

func checkDeployService(x DeployService) error {
	if !validName(x.Name) {
		return fmt.Errorf("%w: name must match [A-Za-z0-9._-]{1,128}", ErrInvalidDeployService)
	}
	if !repoRE.MatchString(x.Repo) {
		return fmt.Errorf("%w: repo must be owner/name", ErrInvalidDeployService)
	}
	if len(x.RecordPrefixes) == 0 {
		return fmt.Errorf("%w: at least one record prefix is required", ErrInvalidDeployService)
	}
	var seen []string
	for _, p := range x.RecordPrefixes {
		segment := strings.TrimSuffix(strings.TrimPrefix(p, "deploy/"), "/")
		valid := validText(p, maxDocumentPrefixBytes) && strings.HasPrefix(p, "deploy/") && strings.HasSuffix(p, "/")
		for _, part := range strings.Split(segment, "/") {
			valid = valid && validName(part)
		}
		if !valid {
			return fmt.Errorf("%w: record prefixes must be unique paths under deploy/ ending in /", ErrInvalidDeployService)
		}
		for _, previous := range seen {
			if strings.HasPrefix(p, previous) || strings.HasPrefix(previous, p) {
				return fmt.Errorf("%w: record prefixes must not overlap", ErrInvalidDeployService)
			}
		}
		seen = append(seen, p)
	}
	return nil
}

const deployServiceColumns = `name,repo,record_prefixes,active,created_at,updated_at`

func scanDeployService(row pgx.Row, x *DeployService) error {
	return row.Scan(&x.Name, &x.Repo, &x.RecordPrefixes, &x.Active, &x.CreatedAt, &x.UpdatedAt)
}

func (s *Store) PutDeployService(ctx context.Context, x DeployService) (DeployService, error) {
	if err := checkDeployService(x); err != nil {
		return DeployService{}, err
	}
	// Serialize prefix claims before checking overlap, including concurrent puts.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return DeployService{}, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('deploy_services_prefixes', 0))`); err != nil {
		return DeployService{}, err
	}
	var conflict bool
	err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM deploy_services s, unnest(s.record_prefixes) AS existing_prefix(prefix), unnest($2::text[]) AS proposed(prefix) WHERE s.active AND s.name <> $1 AND (strpos(existing_prefix.prefix,proposed.prefix)=1 OR strpos(proposed.prefix,existing_prefix.prefix)=1))`, x.Name, x.RecordPrefixes).Scan(&conflict)
	if err != nil {
		return DeployService{}, err
	}
	if conflict {
		return DeployService{}, fmt.Errorf("%w: record prefix overlaps another active service", ErrInvalidDeployService)
	}
	var out DeployService
	err = scanDeployService(tx.QueryRow(ctx, `INSERT INTO deploy_services(name,repo,record_prefixes,active,created_at,updated_at) VALUES($1,$2,$3,true,now(),now()) ON CONFLICT(name) DO UPDATE SET repo=EXCLUDED.repo,record_prefixes=EXCLUDED.record_prefixes,active=true,updated_at=EXCLUDED.updated_at RETURNING `+deployServiceColumns, x.Name, x.Repo, x.RecordPrefixes), &out)
	if err != nil {
		return DeployService{}, err
	}
	return out, tx.Commit(ctx)
}

func (s *Store) ListDeployServices(ctx context.Context) ([]DeployService, error) {
	return listDeployServices(ctx, s.pool)
}

// listDeployServices reads through db, the pool or an open transaction. A pass
// that already holds a transaction reads through it, so it takes no second
// pool connection while its locks are held.
func listDeployServices(ctx context.Context, db rowsQuerier) ([]DeployService, error) {
	rows, err := db.Query(ctx, `SELECT `+deployServiceColumns+` FROM deploy_services ORDER BY name`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (DeployService, error) {
		var x DeployService
		err := scanDeployService(row, &x)
		return x, err
	})
}

func (s *Store) DeactivateDeployService(ctx context.Context, name string) (DeployService, error) {
	if !validName(name) {
		return DeployService{}, fmt.Errorf("%w: name must match [A-Za-z0-9._-]{1,128}", ErrInvalidDeployService)
	}
	var out DeployService
	err := scanDeployService(s.pool.QueryRow(ctx, `UPDATE deploy_services SET active=false,updated_at=now() WHERE name=$1 RETURNING `+deployServiceColumns, name), &out)
	if errors.Is(err, pgx.ErrNoRows) {
		return DeployService{}, ErrServiceNotFound
	}
	return out, err
}
