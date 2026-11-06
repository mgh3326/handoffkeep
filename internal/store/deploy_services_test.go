package store

import (
	"reflect"
	"strings"
	"testing"
)

func TestDeployServiceValidation(t *testing.T) {
	valid := DeployService{Name: "app", Repo: "owner/repo", RecordPrefixes: []string{"deploy/app/"}}
	for name, tc := range map[string]struct {
		x      DeployService
		reason string
	}{
		"name":      {DeployService{Name: "bad/name", Repo: valid.Repo, RecordPrefixes: valid.RecordPrefixes}, "name"},
		"repo":      {DeployService{Name: valid.Name, Repo: "bad", RecordPrefixes: valid.RecordPrefixes}, "repo"},
		"too long":  {DeployService{Name: valid.Name, Repo: valid.Repo, RecordPrefixes: []string{"deploy/" + strings.Repeat(strings.Repeat("a", 128)+"/", 4)}}, "record prefixes"},
		"missing":   {DeployService{Name: valid.Name, Repo: valid.Repo}, "at least one"},
		"prefix":    {DeployService{Name: valid.Name, Repo: valid.Repo, RecordPrefixes: []string{"deploy/app"}}, "record prefixes"},
		"nested":    {DeployService{Name: valid.Name, Repo: valid.Repo, RecordPrefixes: []string{"deploy/app/nested/", "deploy/app/"}}, "overlap"},
		"duplicate": {DeployService{Name: valid.Name, Repo: valid.Repo, RecordPrefixes: []string{"deploy/app/", "deploy/app/"}}, "overlap"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := checkDeployService(tc.x); err == nil || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("validation = %v", err)
			}
		})
	}
	if err := checkDeployService(valid); err != nil {
		t.Fatal(err)
	}
}

func TestDeployServicesMigrationAndCRUD(t *testing.T) {
	s := schemaStore(t, "deploy_services")
	ctx := t.Context()
	xs, err := s.ListDeployServices(ctx)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, x := range xs {
		if !x.Active {
			t.Fatalf("seed inactive: %+v", x)
		}
		names = append(names, x.Name)
	}
	if !reflect.DeepEqual(names, []string{"auto_trader", "handoffkeep", "mergewake", "pacelever", "panewire-hub"}) {
		t.Fatalf("seeds=%v", names)
	}
	if !reflect.DeepEqual(xs[4].RecordPrefixes, []string{"deploy/panewire-hub/", "deploy/panewire/"}) {
		t.Fatalf("aliases=%v", xs[4].RecordPrefixes)
	}
	var recorded bool
	if err = s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE id='20261107-deploy-services')`).Scan(&recorded); err != nil || !recorded {
		t.Fatalf("migration recorded=%v err=%v", recorded, err)
	}
	x, err := s.PutDeployService(ctx, DeployService{Name: "zzz", Repo: "owner/repo", RecordPrefixes: []string{"deploy/zzz/"}})
	if err != nil || !x.Active {
		t.Fatalf("put=%+v %v", x, err)
	}
	if _, err := s.PutDeployService(ctx, DeployService{Name: "other", Repo: "owner/other", RecordPrefixes: []string{"deploy/zzz/nested/"}}); err == nil || !strings.Contains(err.Error(), "overlaps") {
		t.Fatalf("overlap error = %v", err)
	}
	x, err = s.DeactivateDeployService(ctx, "zzz")
	if err != nil || x.Active {
		t.Fatalf("deactivate=%+v %v", x, err)
	}
	x, err = s.PutDeployService(ctx, DeployService{Name: "zzz", Repo: "owner/new", RecordPrefixes: []string{"deploy/new/"}})
	if err != nil || !x.Active || x.Repo != "owner/new" {
		t.Fatalf("reactivate=%+v %v", x, err)
	}
	xs, err = s.ListDeployServices(ctx)
	if err != nil || len(xs) != 6 || xs[5].Name != "zzz" {
		t.Fatalf("list=%+v %v", xs, err)
	}
}
