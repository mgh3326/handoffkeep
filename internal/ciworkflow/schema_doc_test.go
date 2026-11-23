package ciworkflow_test

import (
	"testing"

	"go.yaml.in/yaml/v3"
)

// schema-doc.yml (hk 6106) builds the MySQL schema document on a throwaway
// service container. These guards keep it off pull requests, on the same MySQL
// image as ci.yml's mysql-store job, and on the reviewed tbls checksum.

const reviewedTblsSHA256 = "43c81f408495d9b626d0265a6ebb57c6d1c4a5e072d3eecc3e88825cc6c64e9a"

type schemaDocWorkflow struct {
	On          map[string]any    `yaml:"on"`
	Permissions map[string]string `yaml:"permissions"`
	Jobs        map[string]struct {
		RunsOn   string `yaml:"runs-on"`
		Services map[string]struct {
			Image string `yaml:"image"`
		} `yaml:"services"`
		Steps []step `yaml:"steps"`
	} `yaml:"jobs"`
}

func TestSchemaDocWorkflow(t *testing.T) {
	var wf schemaDocWorkflow
	if err := yaml.Unmarshal(readWorkflow(t, "schema-doc.yml"), &wf); err != nil {
		t.Fatal(err)
	}
	if _, ok := wf.On["pull_request"]; ok {
		t.Error("schema-doc.yml must not trigger on pull_request: pull request CI stays unchanged")
	}
	if _, ok := wf.On["push"]; !ok {
		t.Error("schema-doc.yml must trigger on push to main")
	}
	if _, ok := wf.On["workflow_dispatch"]; !ok {
		t.Error("schema-doc.yml must allow workflow_dispatch")
	}
	if len(wf.Permissions) != 1 || wf.Permissions["contents"] != "read" {
		t.Errorf("schema-doc.yml permissions = %v, want only contents: read", wf.Permissions)
	}

	job, ok := wf.Jobs["schema-doc"]
	if !ok {
		t.Fatal("schema-doc.yml must have a schema-doc job")
	}
	if job.RunsOn != "ubuntu-latest" {
		t.Errorf("schema-doc job runs-on = %q, want ubuntu-latest (GitHub-hosted only)", job.RunsOn)
	}

	var ci struct {
		Jobs map[string]struct {
			Services map[string]struct {
				Image string `yaml:"image"`
			} `yaml:"services"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(readWorkflow(t, "ci.yml"), &ci); err != nil {
		t.Fatal(err)
	}
	want := ci.Jobs["mysql-store"].Services["mysql"].Image
	if want == "" {
		t.Fatal("ci.yml mysql-store job has no mysql service image")
	}
	if got := job.Services["mysql"].Image; got != want {
		t.Errorf("schema-doc mysql image = %q, want the mysql-store image %q", got, want)
	}

	var checksum string
	for _, s := range job.Steps {
		if v, ok := s.Env["TBLS_SHA256"].(string); ok {
			checksum = v
		}
	}
	if checksum != reviewedTblsSHA256 {
		t.Errorf("TBLS_SHA256 = %q, want the reviewed tbls v1.97.0 checksum %q", checksum, reviewedTblsSHA256)
	}
}
