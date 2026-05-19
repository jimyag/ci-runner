package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	HTTPAddr                string
	HTTPReadTimeout         time.Duration
	HTTPWriteTimeout        time.Duration
	HTTPIdleTimeout         time.Duration
	StateDir                string
	AdminToken              string
	E2BAPIKey               string
	E2BAPIURL               string
	E2BDomain               string
	GitHubToken             string
	GitHubAppID             int64
	GitHubAppInstallationID int64
	GitHubAppPrivateKeyFile string
	GitHubWebhookSecret     string
	RunnerScope             string
	GitHubOwner             string
	GitHubOrg               string
	GitHubRepo              string
	SandboxTemplateID       string
	RunnerLabels            []string
	SandboxTimeout          time.Duration
	SandboxAPITimeout       time.Duration
	SandboxCreateTimeout    time.Duration
	SandboxStopTimeout      time.Duration
	RecoveryTimeout         time.Duration
	MaxConcurrentRunners    int
	GitHubAPIBaseURL        string
	ConfigFile              string
	Profiles                []RunnerProfileConfig
	RepositoryPolicies      []RepositoryPolicyConfig
}

type RunnerProfileConfig struct {
	Name           string   `yaml:"name"`
	Labels         []string `yaml:"labels"`
	TemplateID     string   `yaml:"template_id"`
	RunnerGroup    string   `yaml:"runner_group"`
	MaxConcurrency int      `yaml:"max_concurrency"`
	MinIdle        int      `yaml:"min_idle"`
	Priority       int      `yaml:"priority"`
	Enabled        bool     `yaml:"enabled"`
}

type RepositoryPolicyConfig struct {
	Repository      string   `yaml:"repository"`
	AllowedProfiles []string `yaml:"allowed_profiles"`
}

func Load() (Config, error) {
	cfg := Config{
		HTTPAddr:                env("HTTP_ADDR", ":25500"),
		HTTPReadTimeout:         time.Duration(envInt("HTTP_READ_TIMEOUT_SECONDS", 15)) * time.Second,
		HTTPWriteTimeout:        time.Duration(envInt("HTTP_WRITE_TIMEOUT_SECONDS", 60)) * time.Second,
		HTTPIdleTimeout:         time.Duration(envInt("HTTP_IDLE_TIMEOUT_SECONDS", 120)) * time.Second,
		StateDir:                env("STATE_DIR", "./var/runners"),
		AdminToken:              os.Getenv("ADMIN_TOKEN"),
		E2BAPIKey:               os.Getenv("E2B_API_KEY"),
		E2BAPIURL:               os.Getenv("E2B_API_URL"),
		E2BDomain:               os.Getenv("E2B_DOMAIN"),
		GitHubToken:             os.Getenv("GITHUB_TOKEN"),
		GitHubAppID:             envInt64("GITHUB_APP_ID", 0),
		GitHubAppInstallationID: envInt64("GITHUB_APP_INSTALLATION_ID", 0),
		GitHubAppPrivateKeyFile: os.Getenv("GITHUB_APP_PRIVATE_KEY_FILE"),
		GitHubWebhookSecret:     os.Getenv("GITHUB_WEBHOOK_SECRET"),
		RunnerScope:             strings.ToLower(env("RUNNER_SCOPE", "repo")),
		GitHubOwner:             os.Getenv("GITHUB_OWNER"),
		GitHubOrg:               os.Getenv("GITHUB_ORG"),
		GitHubRepo:              os.Getenv("GITHUB_REPO"),
		SandboxTemplateID:       os.Getenv("SANDBOX_TEMPLATE_ID"),
		RunnerLabels:            splitLabels(env("RUNNER_LABELS", "self-hosted,e2b")),
		SandboxTimeout:          time.Duration(envInt("SANDBOX_TIMEOUT_SECONDS", 3600)) * time.Second,
		SandboxAPITimeout:       time.Duration(envInt("SANDBOX_API_TIMEOUT_SECONDS", 60)) * time.Second,
		SandboxCreateTimeout:    time.Duration(envInt("SANDBOX_CREATE_TIMEOUT_SECONDS", 120)) * time.Second,
		SandboxStopTimeout:      time.Duration(envInt("SANDBOX_STOP_TIMEOUT_SECONDS", 30)) * time.Second,
		RecoveryTimeout:         time.Duration(envInt("RECOVERY_TIMEOUT_SECONDS", 120)) * time.Second,
		MaxConcurrentRunners:    envInt("MAX_CONCURRENT_RUNNERS", 100),
		GitHubAPIBaseURL:        env("GITHUB_API_BASE_URL", "https://api.github.com"),
		ConfigFile:              os.Getenv("RUNNERD_CONFIG_FILE"),
	}
	if err := cfg.loadSeedConfig(); err != nil {
		return Config{}, err
	}
	var missing []string
	for name, value := range map[string]string{
		"ADMIN_TOKEN":           cfg.AdminToken,
		"E2B_API_KEY":           cfg.E2BAPIKey,
		"E2B_API_URL":           cfg.E2BAPIURL,
		"E2B_DOMAIN":            cfg.E2BDomain,
		"GITHUB_WEBHOOK_SECRET": cfg.GitHubWebhookSecret,
		"SANDBOX_TEMPLATE_ID":   cfg.SandboxTemplateID,
	} {
		if value == "" {
			missing = append(missing, name)
		}
	}
	if cfg.GitHubToken == "" {
		if cfg.GitHubAppID == 0 {
			missing = append(missing, "GITHUB_TOKEN or GITHUB_APP_ID")
		}
		if cfg.GitHubAppInstallationID == 0 {
			missing = append(missing, "GITHUB_APP_INSTALLATION_ID")
		}
		if cfg.GitHubAppPrivateKeyFile == "" {
			missing = append(missing, "GITHUB_APP_PRIVATE_KEY_FILE")
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
	cfg.ensureDefaultSeeds()
	return cfg, nil
}

func (c Config) GitHubAuthMode() string {
	if c.GitHubToken != "" {
		return "token"
	}
	return "app"
}

func (c Config) DefaultRepositoryPattern() string {
	if c.RunnerScope == "org" && c.GitHubOrg != "" {
		return c.GitHubOrg + "/*"
	}
	if c.GitHubOwner != "" && c.GitHubRepo != "" {
		return c.GitHubOwner + "/" + c.GitHubRepo
	}
	return ""
}

func (c *Config) loadSeedConfig() error {
	if strings.TrimSpace(c.ConfigFile) == "" {
		return nil
	}
	data, err := os.ReadFile(c.ConfigFile)
	if err != nil {
		return err
	}
	var file struct {
		Profiles           []RunnerProfileConfig    `yaml:"profiles"`
		RepositoryPolicies []RepositoryPolicyConfig `yaml:"repository_policies"`
	}
	if err := yaml.Unmarshal(data, &file); err != nil {
		return err
	}
	c.Profiles = file.Profiles
	c.RepositoryPolicies = file.RepositoryPolicies
	return nil
}

func (c *Config) ensureDefaultSeeds() {
	if len(c.Profiles) == 0 {
		c.Profiles = []RunnerProfileConfig{{
			Name:           "default",
			Labels:         append([]string(nil), c.RunnerLabels...),
			TemplateID:     c.SandboxTemplateID,
			RunnerGroup:    "default",
			MaxConcurrency: c.MaxConcurrentRunners,
			Enabled:        true,
		}}
	}
	if len(c.RepositoryPolicies) == 0 {
		if repository := c.DefaultRepositoryPattern(); repository != "" {
			c.RepositoryPolicies = []RepositoryPolicyConfig{{
				Repository:      repository,
				AllowedProfiles: []string{"default"},
			}}
		}
	}
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

func envInt64(name string, fallback int64) int64 {
	v := os.Getenv(name)
	if v == "" {
		return fallback
	}
	n, err := strconv.ParseInt(v, 10, 64)
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
