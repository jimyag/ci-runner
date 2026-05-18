package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	HTTPAddr             string
	StateDir             string
	E2BAPIKey            string
	E2BAPIURL            string
	E2BDomain            string
	GitHubToken          string
	GitHubWebhookSecret  string
	RunnerScope          string
	GitHubOwner          string
	GitHubOrg            string
	GitHubRepo           string
	SandboxTemplateID    string
	RunnerLabels         []string
	RunnerVersion        string
	SandboxTimeout       time.Duration
	MaxConcurrentRunners int
	GitHubAPIBaseURL     string
}

func Load() (Config, error) {
	cfg := Config{
		HTTPAddr:             env("HTTP_ADDR", ":8080"),
		StateDir:             env("STATE_DIR", "./var/runners"),
		E2BAPIKey:            os.Getenv("E2B_API_KEY"),
		E2BAPIURL:            os.Getenv("E2B_API_URL"),
		E2BDomain:            os.Getenv("E2B_DOMAIN"),
		GitHubToken:          os.Getenv("GITHUB_TOKEN"),
		GitHubWebhookSecret:  os.Getenv("GITHUB_WEBHOOK_SECRET"),
		RunnerScope:          strings.ToLower(env("RUNNER_SCOPE", "repo")),
		GitHubOwner:          os.Getenv("GITHUB_OWNER"),
		GitHubOrg:            os.Getenv("GITHUB_ORG"),
		GitHubRepo:           os.Getenv("GITHUB_REPO"),
		SandboxTemplateID:    os.Getenv("SANDBOX_TEMPLATE_ID"),
		RunnerLabels:         splitLabels(env("RUNNER_LABELS", "self-hosted,e2b")),
		RunnerVersion:        env("RUNNER_VERSION", "2.334.0"),
		SandboxTimeout:       time.Duration(envInt("SANDBOX_TIMEOUT_SECONDS", 3600)) * time.Second,
		MaxConcurrentRunners: envInt("MAX_CONCURRENT_RUNNERS", 1),
		GitHubAPIBaseURL:     env("GITHUB_API_BASE_URL", "https://api.github.com"),
	}
	var missing []string
	for name, value := range map[string]string{
		"E2B_API_KEY":           cfg.E2BAPIKey,
		"E2B_API_URL":           cfg.E2BAPIURL,
		"E2B_DOMAIN":            cfg.E2BDomain,
		"GITHUB_TOKEN":          cfg.GitHubToken,
		"GITHUB_WEBHOOK_SECRET": cfg.GitHubWebhookSecret,
		"SANDBOX_TEMPLATE_ID":   cfg.SandboxTemplateID,
	} {
		if value == "" {
			missing = append(missing, name)
		}
	}
	switch cfg.RunnerScope {
	case "repo":
		if cfg.GitHubOwner == "" {
			missing = append(missing, "GITHUB_OWNER")
		}
		if cfg.GitHubRepo == "" {
			missing = append(missing, "GITHUB_REPO")
		}
	case "org":
		if cfg.GitHubOrg == "" {
			cfg.GitHubOrg = cfg.GitHubOwner
		}
		if cfg.GitHubOrg == "" {
			missing = append(missing, "GITHUB_ORG")
		}
	default:
		return Config{}, fmt.Errorf("RUNNER_SCOPE must be repo or org")
	}
	if len(missing) > 0 {
		return Config{}, fmt.Errorf("missing required env: %s", strings.Join(missing, ", "))
	}
	if cfg.MaxConcurrentRunners < 1 {
		return Config{}, fmt.Errorf("MAX_CONCURRENT_RUNNERS must be >= 1")
	}
	if len(cfg.RunnerLabels) == 0 {
		return Config{}, fmt.Errorf("RUNNER_LABELS must include at least one label")
	}
	return cfg, nil
}

func env(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func envInt(name string, fallback int) int {
	v := os.Getenv(name)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

func splitLabels(value string) []string {
	seen := map[string]bool{}
	var labels []string
	for _, part := range strings.Split(value, ",") {
		label := strings.TrimSpace(part)
		if label == "" || seen[label] {
			continue
		}
		seen[label] = true
		labels = append(labels, label)
	}
	return labels
}
