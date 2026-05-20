package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadFileUsesRunnerdYAML(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "runnerd.yaml")
	if err := os.WriteFile(configPath, []byte(`
server:
  http_addr: ":28000"
database:
  backend: sqlite
  url: `+filepath.Join(dir, "runnerd.db")+`
admin:
  token: admin-token
e2b:
  api_key: test-key
  api_url: https://api.e2b.dev
  domain: example.e2b.dev
  template_id: base
github:
  webhook_secret: webhook-secret
  api_base_url: https://api.github.example
  scope: repo
  app:
    id: 123
    installation_id: 456
    private_key_file: ./secrets/app.pem
worker:
  runner_labels:
    - self-hosted
    - e2b
  max_concurrent_runners: 5
runner_specs:
  - name: large
    labels: [self-hosted, e2b, large]
    template_id: large
    runner_group: default
    max_concurrency: 2
    default_available: true
    enabled: true
runner_policies:
  - repository: octo/repo
    allowed_specs: [large]
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPAddr != ":28000" {
		t.Fatalf("unexpected HTTP address: %s", cfg.HTTPAddr)
	}
	if cfg.StateBackend != "sqlite" {
		t.Fatalf("unexpected state backend: %s", cfg.StateBackend)
	}
	if cfg.StateDatabaseURL != filepath.Join(dir, "runnerd.db") {
		t.Fatalf("unexpected database url: %s", cfg.StateDatabaseURL)
	}
	if cfg.GitHubAuthMode() != "app" {
		t.Fatalf("unexpected auth mode: %s", cfg.GitHubAuthMode())
	}
	if cfg.DefaultRepositoryPattern() != "" {
		t.Fatalf("repo scope should not require a default repository, got %q", cfg.DefaultRepositoryPattern())
	}
	if len(cfg.RunnerSpecs) != 1 || cfg.RunnerSpecs[0].Name != "large" {
		t.Fatalf("unexpected runner_specs: %#v", cfg.RunnerSpecs)
	}
	if !cfg.RunnerSpecs[0].DefaultAvailable {
		t.Fatalf("expected runner spec to be globally available")
	}
	if len(cfg.RunnerPolicies) != 1 || cfg.RunnerPolicies[0].Repository != "octo/repo" {
		t.Fatalf("unexpected repository policies: %#v", cfg.RunnerPolicies)
	}
}

func TestLoadFileRequiresGitHubAppConfigPaths(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "runnerd.yaml")
	if err := os.WriteFile(configPath, []byte(`
database:
  backend: sqlite
  url: `+filepath.Join(dir, "runnerd.db")+`
admin:
  token: admin-token
e2b:
  api_key: test-key
  api_url: https://api.e2b.dev
  domain: example.e2b.dev
  template_id: base
github:
  webhook_secret: webhook-secret
  scope: repo
worker:
  runner_labels: [self-hosted, e2b]
`), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadFile(configPath)
	if err == nil {
		t.Fatal("expected validation error")
	}
	for _, want := range []string{"github.app.id", "github.app.installation_id", "github.app.private_key_file"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("expected error to mention %s, got %v", want, err)
		}
	}
}
