package config

import (
	"fmt"
	"os"
	"path/filepath"
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
	StateBackend            string
	StateDatabaseURL        string
	AdminToken              string
	E2BAPIKey               string
	E2BAPIURL               string
	E2BDomain               string
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
	WorkerLeaseTTL          time.Duration
	RunnerIdleTimeout       time.Duration
	RetryBaseDelay          time.Duration
	RetryMaxDelay           time.Duration
	RetryMaxAttempts        int
	MaxConcurrentRunners    int
	GitHubAPIBaseURL        string
	ConfigPath              string
	RunnerSpecs             []RunnerSpecConfig
	RunnerGroups            []RunnerGroupConfig
	RunnerPolicies          []RunnerPolicyConfig
}

type RunnerSpecConfig struct {
	Name             string   `yaml:"name"`
	Labels           []string `yaml:"labels"`
	TemplateID       string   `yaml:"template_id"`
	RunnerGroup      string   `yaml:"runner_group"`
	MaxConcurrency   int      `yaml:"max_concurrency"`
	MinIdle          int      `yaml:"min_idle"`
	Priority         int      `yaml:"priority"`
	Enabled          bool     `yaml:"enabled"`
	DefaultAvailable bool     `yaml:"default_available"`
}

type RunnerPolicyConfig struct {
	Repository    string   `yaml:"repository"`
	AllowedSpecs  []string `yaml:"allowed_specs"`
	AllowedGroups []string `yaml:"allowed_groups"`
}

type RunnerGroupConfig struct {
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	SpecNames   []string `yaml:"spec_names"`
	Enabled     bool     `yaml:"enabled"`
}

type fileConfig struct {
	Server struct {
		HTTPAddr        string `yaml:"http_addr"`
		ReadTimeoutSec  int    `yaml:"read_timeout_seconds"`
		WriteTimeoutSec int    `yaml:"write_timeout_seconds"`
		IdleTimeoutSec  int    `yaml:"idle_timeout_seconds"`
	} `yaml:"server"`
	Database struct {
		Backend string `yaml:"backend"`
		URL     string `yaml:"url"`
	} `yaml:"database"`
	Admin struct {
		Token string `yaml:"token"`
	} `yaml:"admin"`
	E2B struct {
		APIKey           string `yaml:"api_key"`
		APIURL           string `yaml:"api_url"`
		Domain           string `yaml:"domain"`
		TemplateID       string `yaml:"template_id"`
		TimeoutSec       int    `yaml:"timeout_seconds"`
		APITimeoutSec    int    `yaml:"api_timeout_seconds"`
		CreateTimeoutSec int    `yaml:"create_timeout_seconds"`
		StopTimeoutSec   int    `yaml:"stop_timeout_seconds"`
	} `yaml:"e2b"`
	GitHub struct {
		WebhookSecret string `yaml:"webhook_secret"`
		APIBaseURL    string `yaml:"api_base_url"`
		Scope         string `yaml:"scope"`
		Owner         string `yaml:"owner"`
		Org           string `yaml:"org"`
		Repo          string `yaml:"repo"`
		App           struct {
			ID             int64  `yaml:"id"`
			InstallationID int64  `yaml:"installation_id"`
			PrivateKeyFile string `yaml:"private_key_file"`
		} `yaml:"app"`
	} `yaml:"github"`
	Worker struct {
		RunnerLabels         []string `yaml:"runner_labels"`
		MaxConcurrentRunners int      `yaml:"max_concurrent_runners"`
		RunnerIdleTimeoutSec int      `yaml:"runner_idle_timeout_seconds"`
		RecoveryTimeoutSec   int      `yaml:"recovery_timeout_seconds"`
		LeaseTTLSec          int      `yaml:"lease_ttl_seconds"`
		RetryBaseDelaySec    int      `yaml:"retry_base_delay_seconds"`
		RetryMaxDelaySec     int      `yaml:"retry_max_delay_seconds"`
		RetryMaxAttempts     int      `yaml:"retry_max_attempts"`
	} `yaml:"worker"`
	RunnerSpecs    []RunnerSpecConfig   `yaml:"runner_specs"`
	RunnerGroups   []RunnerGroupConfig  `yaml:"runner_groups"`
	RunnerPolicies []RunnerPolicyConfig `yaml:"runner_policies"`
}

func Load(path string) (Config, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		path = "runnerd.yaml"
	}
	return LoadFile(path)
}

func LoadFile(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config %q: %w", path, err)
	}
	var raw fileConfig
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return Config{}, fmt.Errorf("parse config %q: %w", path, err)
	}
	cfg := Config{
		HTTPAddr:                defaultString(raw.Server.HTTPAddr, ":25500"),
		HTTPReadTimeout:         durationSeconds(raw.Server.ReadTimeoutSec, 15),
		HTTPWriteTimeout:        durationSeconds(raw.Server.WriteTimeoutSec, 60),
		HTTPIdleTimeout:         durationSeconds(raw.Server.IdleTimeoutSec, 120),
		StateBackend:            strings.ToLower(defaultString(raw.Database.Backend, "sqlite")),
		StateDatabaseURL:        raw.Database.URL,
		AdminToken:              raw.Admin.Token,
		E2BAPIKey:               raw.E2B.APIKey,
		E2BAPIURL:               raw.E2B.APIURL,
		E2BDomain:               raw.E2B.Domain,
		GitHubAppID:             raw.GitHub.App.ID,
		GitHubAppInstallationID: raw.GitHub.App.InstallationID,
		GitHubAppPrivateKeyFile: raw.GitHub.App.PrivateKeyFile,
		GitHubWebhookSecret:     raw.GitHub.WebhookSecret,
		RunnerScope:             strings.ToLower(defaultString(raw.GitHub.Scope, "repo")),
		GitHubOwner:             raw.GitHub.Owner,
		GitHubOrg:               raw.GitHub.Org,
		GitHubRepo:              raw.GitHub.Repo,
		SandboxTemplateID:       raw.E2B.TemplateID,
		RunnerLabels:            normalizeLabels(raw.Worker.RunnerLabels),
		SandboxTimeout:          durationSeconds(raw.E2B.TimeoutSec, 3600),
		SandboxAPITimeout:       durationSeconds(raw.E2B.APITimeoutSec, 60),
		SandboxCreateTimeout:    durationSeconds(raw.E2B.CreateTimeoutSec, 120),
		SandboxStopTimeout:      durationSeconds(raw.E2B.StopTimeoutSec, 30),
		RecoveryTimeout:         durationSeconds(raw.Worker.RecoveryTimeoutSec, 120),
		WorkerLeaseTTL:          durationSeconds(raw.Worker.LeaseTTLSec, 300),
		RunnerIdleTimeout:       durationSeconds(raw.Worker.RunnerIdleTimeoutSec, 300),
		RetryBaseDelay:          durationSeconds(raw.Worker.RetryBaseDelaySec, 15),
		RetryMaxDelay:           durationSeconds(raw.Worker.RetryMaxDelaySec, 300),
		RetryMaxAttempts:        defaultInt(raw.Worker.RetryMaxAttempts, 5),
		MaxConcurrentRunners:    defaultInt(raw.Worker.MaxConcurrentRunners, 100),
		GitHubAPIBaseURL:        defaultString(raw.GitHub.APIBaseURL, "https://api.github.com"),
		ConfigPath:              path,
		RunnerSpecs:             raw.RunnerSpecs,
		RunnerGroups:            raw.RunnerGroups,
		RunnerPolicies:          raw.RunnerPolicies,
	}
	if cfg.RunnerScope == "org" && cfg.GitHubOrg == "" {
		cfg.GitHubOrg = cfg.GitHubOwner
	}
	if cfg.StateBackend == "sqlite" && strings.TrimSpace(cfg.StateDatabaseURL) == "" {
		cfg.StateDatabaseURL = filepath.Join(".", "var", "runnerd.db")
	}
	if cfg.StateBackend == "sqlite" {
		cfg.StateDir = filepath.Dir(cfg.StateDatabaseURL)
	}
	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	cfg.ensureDefaultSeeds()
	return cfg, nil
}

func (c Config) GitHubAuthMode() string {
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

func (c *Config) ensureDefaultSeeds() {
	if len(c.RunnerSpecs) == 0 {
		c.RunnerSpecs = []RunnerSpecConfig{{
			Name:             "default",
			Labels:           append([]string(nil), c.RunnerLabels...),
			TemplateID:       c.SandboxTemplateID,
			RunnerGroup:      "default",
			MaxConcurrency:   c.MaxConcurrentRunners,
			Enabled:          true,
			DefaultAvailable: true,
		}}
	}
	if len(c.RunnerGroups) == 0 {
		c.RunnerGroups = []RunnerGroupConfig{{
			Name:      "default",
			SpecNames: []string{"default"},
			Enabled:   true,
		}}
	}
	if len(c.RunnerPolicies) == 0 {
		if repository := c.DefaultRepositoryPattern(); repository != "" {
			c.RunnerPolicies = []RunnerPolicyConfig{{
				Repository:    repository,
				AllowedGroups: []string{"default"},
			}}
		}
	}
}

func (c Config) validate() error {
	var missing []string
	for path, value := range map[string]string{
		"admin.token":                 c.AdminToken,
		"e2b.api_key":                 c.E2BAPIKey,
		"e2b.api_url":                 c.E2BAPIURL,
		"e2b.domain":                  c.E2BDomain,
		"e2b.template_id":             c.SandboxTemplateID,
		"github.webhook_secret":       c.GitHubWebhookSecret,
		"github.app.private_key_file": c.GitHubAppPrivateKeyFile,
	} {
		if strings.TrimSpace(value) == "" {
			missing = append(missing, path)
		}
	}
	if c.GitHubAppID == 0 {
		missing = append(missing, "github.app.id")
	}
	if c.GitHubAppInstallationID == 0 {
		missing = append(missing, "github.app.installation_id")
	}
	switch c.RunnerScope {
	case "repo":
	case "org":
		if c.GitHubOrg == "" {
			missing = append(missing, "github.org")
		}
	default:
		return fmt.Errorf("invalid config github.scope: must be repo or org")
	}
	if c.StateBackend != "sqlite" && c.StateBackend != "postgres" {
		return fmt.Errorf("invalid config database.backend: must be sqlite or postgres")
	}
	if strings.TrimSpace(c.StateDatabaseURL) == "" {
		missing = append(missing, "database.url")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required config: %s", strings.Join(missing, ", "))
	}
	if c.MaxConcurrentRunners < 1 {
		return fmt.Errorf("invalid config worker.max_concurrent_runners: must be >= 1")
	}
	if c.RetryMaxAttempts < 1 {
		return fmt.Errorf("invalid config worker.retry_max_attempts: must be >= 1")
	}
	if len(c.RunnerLabels) == 0 {
		return fmt.Errorf("invalid config worker.runner_labels: must include at least one label")
	}
	return nil
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func defaultInt(value, fallback int) int {
	if value <= 0 {
		return fallback
	}
	return value
}

func durationSeconds(value, fallback int) time.Duration {
	if value <= 0 {
		value = fallback
	}
	return time.Duration(value) * time.Second
}

func normalizeLabels(values []string) []string {
	seen := map[string]bool{}
	var labels []string
	for _, value := range values {
		label := strings.TrimSpace(value)
		if label == "" || seen[label] {
			continue
		}
		seen[label] = true
		labels = append(labels, label)
	}
	return labels
}
