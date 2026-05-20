package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"time"

	_ "github.com/jimmicro/pprof"
	"github.com/jimyag/e2b-github-runner/internal/config"
	"github.com/jimyag/e2b-github-runner/internal/github"
	"github.com/jimyag/e2b-github-runner/internal/sandboxrunner"
	"github.com/jimyag/e2b-github-runner/internal/server"
	"github.com/jimyag/e2b-github-runner/internal/state"
)

func main() {
	configPath := flag.String("config", "runnerd.yaml", "path to runnerd config file")
	flag.Parse()
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Error("load config", "error", err)
		os.Exit(1)
	}
	store := state.NewWithOptions(state.Options{
		Backend:        cfg.StateBackend,
		DatabaseURL:    cfg.StateDatabaseURL,
		MigrateOnStart: true,
	})
	if err := store.Ensure(); err != nil {
		logger.Error("ensure state store", "error", err)
		os.Exit(1)
	}
	if err := seedRunnerSpecsAndPolicies(store, cfg); err != nil {
		logger.Error("seed runner specs and policies", "error", err)
		os.Exit(1)
	}
	githubHTTPClient := &http.Client{Timeout: 30 * time.Second}
	sandboxHTTPClient := &http.Client{}
	gh, err := github.NewAppClient(cfg.GitHubAPIBaseURL, cfg.RunnerScope, cfg.GitHubOwner, cfg.GitHubOrg, cfg.GitHubRepo, github.AppAuth{
		AppID:          cfg.GitHubAppID,
		InstallationID: cfg.GitHubAppInstallationID,
		PrivateKeyFile: cfg.GitHubAppPrivateKeyFile,
	}, githubHTTPClient)
	if err != nil {
		logger.Error("create github app client", "error", err)
		os.Exit(1)
	}
	sb, err := sandboxrunner.NewE2BService(cfg.E2BAPIKey, cfg.E2BAPIURL, sandboxHTTPClient)
	if err != nil {
		logger.Error("create sandbox client", "error", err)
		os.Exit(1)
	}
	handler := server.New(cfg, store, gh, sb, logger)
	recoveryCtx, cancel := context.WithTimeout(context.Background(), cfg.RecoveryTimeout)
	if err := handler.Recover(recoveryCtx); err != nil {
		cancel()
		logger.Error("recover runner state", "error", err)
		os.Exit(1)
	}
	cancel()
	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       cfg.HTTPReadTimeout,
		WriteTimeout:      cfg.HTTPWriteTimeout,
		IdleTimeout:       cfg.HTTPIdleTimeout,
		MaxHeaderBytes:    1 << 20,
	}
	logger.Info("starting server", "addr", cfg.HTTPAddr, "state_backend", cfg.StateBackend, "state_database_url", cfg.StateDatabaseURL)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

func seedRunnerSpecsAndPolicies(store state.Store, cfg config.Config) error {
	for _, spec := range cfg.RunnerSpecs {
		if _, err := store.GetProfile(spec.Name); err == nil {
			continue
		}
		if _, err := store.UpsertProfile(state.RunnerProfile{
			Name:             spec.Name,
			Labels:           spec.Labels,
			TemplateID:       spec.TemplateID,
			RunnerGroup:      spec.RunnerGroup,
			MaxConcurrency:   spec.MaxConcurrency,
			MinIdle:          spec.MinIdle,
			Priority:         spec.Priority,
			Enabled:          spec.Enabled,
			DefaultAvailable: spec.DefaultAvailable,
		}); err != nil {
			return err
		}
	}
	for _, group := range cfg.RunnerGroups {
		if _, err := store.GetRunnerGroup(group.Name); err == nil {
			continue
		}
		if _, err := store.UpsertRunnerGroup(state.RunnerGroup{
			Name:        group.Name,
			Description: group.Description,
			SpecNames:   group.SpecNames,
			Enabled:     group.Enabled,
		}); err != nil {
			return err
		}
	}
	existingPolicies, err := store.ListRepositoryPolicies()
	if err != nil {
		return err
	}
	for _, policy := range cfg.RunnerPolicies {
		for _, specName := range policy.AllowedSpecs {
			if repositoryPolicyExists(existingPolicies, policy.Repository, specName) {
				continue
			}
			if _, err := store.UpsertRepositoryPolicy(state.RepositoryPolicy{
				RepositoryFullName: policy.Repository,
				ProfileName:        specName,
				Enabled:            true,
			}); err != nil {
				return err
			}
		}
		for _, groupName := range policy.AllowedGroups {
			if repositoryGroupPolicyExists(existingPolicies, policy.Repository, groupName) {
				continue
			}
			if _, err := store.UpsertRepositoryPolicy(state.RepositoryPolicy{
				RepositoryFullName: policy.Repository,
				RunnerGroupName:    groupName,
				Enabled:            true,
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

func repositoryPolicyExists(policies []state.RepositoryPolicy, repository, profileName string) bool {
	for _, policy := range policies {
		if policy.RepositoryFullName == repository && policy.ProfileName == profileName {
			return true
		}
	}
	return false
}

func repositoryGroupPolicyExists(policies []state.RepositoryPolicy, repository, groupName string) bool {
	for _, policy := range policies {
		if policy.RepositoryFullName == repository && policy.RunnerGroupName == groupName {
			return true
		}
	}
	return false
}
