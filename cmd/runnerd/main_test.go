package main

import (
	"testing"

	"github.com/jimyag/e2b-github-runner/internal/config"
	"github.com/jimyag/e2b-github-runner/internal/state"
)

func TestSeedRunnerSpecsAndPoliciesDoesNotOverwriteExistingEntries(t *testing.T) {
	store := state.New(t.TempDir())
	if err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertProfile(state.RunnerProfile{
		Name:           "default",
		Labels:         []string{"self-hosted", "custom"},
		TemplateID:     "existing-template",
		RunnerGroup:    "existing",
		MaxConcurrency: 7,
		Enabled:        true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertRepositoryPolicy(state.RepositoryPolicy{
		RepositoryFullName: "o/r",
		ProfileName:        "default",
		Enabled:            false,
	}); err != nil {
		t.Fatal(err)
	}

	cfg := config.Config{
		RunnerSpecs: []config.RunnerSpecConfig{{
			Name:           "default",
			Labels:         []string{"self-hosted", "e2b"},
			TemplateID:     "new-template",
			RunnerGroup:    "new-group",
			MaxConcurrency: 99,
			Enabled:        true,
		}},
		RunnerPolicies: []config.RunnerPolicyConfig{{
			Repository:   "o/r",
			AllowedSpecs: []string{"default"},
		}},
	}

	if err := seedRunnerSpecsAndPolicies(store, cfg); err != nil {
		t.Fatal(err)
	}

	profile, err := store.GetProfile("default")
	if err != nil {
		t.Fatal(err)
	}
	if profile.TemplateID != "existing-template" || profile.RunnerGroup != "existing" || profile.MaxConcurrency != 7 {
		t.Fatalf("existing profile was overwritten: %#v", profile)
	}
	policies, err := store.ListRepositoryPolicies()
	if err != nil {
		t.Fatal(err)
	}
	if len(policies) != 1 || policies[0].Enabled != false {
		t.Fatalf("existing policy was overwritten: %#v", policies)
	}
}
