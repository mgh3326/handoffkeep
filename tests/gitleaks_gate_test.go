package tests

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/BurntSushi/toml"
	"go.yaml.in/yaml/v3"
)

// .gitleaks.toml must explicitly enable the default rule set. gitleaks v8 uses
// only the rules present in a supplied config file, so a config without
// extend.useDefault leaves the repository leak gate scanning with zero rules.
func TestGitleaksConfigEnablesDefaultRules(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", ".gitleaks.toml"))
	if err != nil {
		t.Fatalf("read .gitleaks.toml: %v", err)
	}
	var cfg struct {
		Extend struct {
			UseDefault bool `toml:"useDefault"`
		} `toml:"extend"`
	}
	meta, err := toml.Decode(string(body), &cfg)
	if err != nil {
		t.Fatalf("parse .gitleaks.toml: %v", err)
	}
	if !meta.IsDefined("extend", "useDefault") {
		t.Fatal(".gitleaks.toml does not define [extend] useDefault")
	}
	if cfg.Extend.UseDefault != true {
		t.Fatalf(".gitleaks.toml extend.useDefault = %v, want exactly true", cfg.Extend.UseDefault)
	}
}

// A global gitleaks allowlist entry (one without targetRules) must not scope
// findings by paths, regexes, or stopwords. Global regexes/stopwords suppress
// matching findings anywhere in the repo, and global paths make the directory
// scanner skip the whole file regardless of the entry's condition. Scoping
// belongs on rule-scoped [[allowlists]] entries (targetRules + condition=AND).
func TestGitleaksConfigHasNoUnscopedGlobalAllowlist(t *testing.T) {
	type allowlist struct {
		Condition   string   `toml:"condition"`
		TargetRules []string `toml:"targetRules"`
		Paths       []string `toml:"paths"`
		Regexes     []string `toml:"regexes"`
		StopWords   []string `toml:"stopwords"`
	}
	var cfg struct {
		AllowList  *allowlist  `toml:"allowlist"`
		Allowlists []allowlist `toml:"allowlists"`
	}
	body, err := os.ReadFile(filepath.Join("..", ".gitleaks.toml"))
	if err != nil {
		t.Fatalf("read .gitleaks.toml: %v", err)
	}
	if _, err := toml.Decode(string(body), &cfg); err != nil {
		t.Fatalf("parse .gitleaks.toml: %v", err)
	}
	all := cfg.Allowlists
	if cfg.AllowList != nil {
		all = append(all, *cfg.AllowList)
	}
	for i, a := range all {
		if len(a.TargetRules) > 0 {
			continue
		}
		if len(a.Paths) > 0 || len(a.Regexes) > 0 || len(a.StopWords) > 0 {
			t.Fatalf("global allowlist[%d] scopes findings by paths/regexes/stopwords; "+
				"use [[allowlists]] with targetRules instead", i)
		}
	}
}

// The CI gitleaks step must name the repository config file instead of relying
// on auto-discovery, so the gate cannot silently fall back to another config.
func TestCIGitleaksStepSpecifiesRepoConfig(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatalf("read ci.yml: %v", err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct {
				Uses string            `yaml:"uses"`
				Env  map[string]string `yaml:"env"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(body, &workflow); err != nil {
		t.Fatalf("parse ci.yml: %v", err)
	}
	steps := 0
	for jobName, job := range workflow.Jobs {
		for _, step := range job.Steps {
			if step.Uses == "" || step.Uses[:1] == "." {
				continue
			}
			if len(step.Uses) < len("gitleaks/gitleaks-action@") ||
				step.Uses[:len("gitleaks/gitleaks-action@")] != "gitleaks/gitleaks-action@" {
				continue
			}
			steps++
			if got := step.Env["GITLEAKS_CONFIG"]; got != ".gitleaks.toml" {
				t.Fatalf("job %q gitleaks step GITLEAKS_CONFIG=%q, want .gitleaks.toml", jobName, got)
			}
		}
	}
	if steps == 0 {
		t.Fatal("ci.yml has no gitleaks/gitleaks-action step")
	}
}
