package state

import (
	"errors"
	"testing"
	"time"
)

func TestCreateRequestIsIdempotent(t *testing.T) {
	store := New(t.TempDir())
	req := RunnerRequest{ID: "123", Source: "test", Labels: []string{"self-hosted"}, RunnerName: "e2b-123"}
	created, st, err := store.CreateRequest(req, []byte(`{"x":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if !created || st.Status != StatusQueued {
		t.Fatalf("unexpected first create: created=%v state=%#v", created, st)
	}

	created, st, err = store.CreateRequest(req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("expected duplicate request to reuse existing row")
	}
	if st.ID != "123" {
		t.Fatalf("unexpected state id: %q", st.ID)
	}
}

func TestWriteStateUsesVersionCAS(t *testing.T) {
	store := New(t.TempDir())
	_, st, err := store.CreateRequest(RunnerRequest{
		ID:         "123",
		Source:     "test",
		Labels:     []string{"self-hosted"},
		RunnerName: "e2b-123",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	st.Status = StatusRunning
	st.ProcessPID = 42
	if err := store.WriteState(st); err != nil {
		t.Fatal(err)
	}

	st.Status = StatusFailed
	err = store.WriteState(st)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("expected version conflict, got %v", err)
	}
}

func TestListStatesAndActiveCount(t *testing.T) {
	store := New(t.TempDir())
	if _, _, err := store.CreateRequest(RunnerRequest{
		ID:         "active",
		Source:     "test",
		Labels:     []string{"self-hosted"},
		RunnerName: "e2b-active",
	}, nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if _, st, err := store.CreateRequest(RunnerRequest{
		ID:         "done",
		Source:     "test",
		Labels:     []string{"self-hosted"},
		RunnerName: "e2b-done",
	}, nil); err != nil {
		t.Fatal(err)
	} else {
		st.Status = StatusCompleted
		if err := store.WriteState(st); err != nil {
			t.Fatal(err)
		}
	}

	states, err := store.ListStates()
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 2 {
		t.Fatalf("expected 2 states, got %d", len(states))
	}
	if states[0].ID != "done" {
		t.Fatalf("expected newest state first, got %q", states[0].ID)
	}
	active, err := store.ActiveCount()
	if err != nil {
		t.Fatal(err)
	}
	if active != 1 {
		t.Fatalf("expected 1 active runner, got %d", active)
	}
}

func TestReadLogCanReturnTail(t *testing.T) {
	store := New(t.TempDir())
	if _, _, err := store.CreateRequest(RunnerRequest{
		ID:         "123",
		Source:     "test",
		Labels:     []string{"self-hosted"},
		RunnerName: "e2b-123",
	}, nil); err != nil {
		t.Fatal(err)
	}
	store.AppendLog("123", "control.log", []byte("line-1\n"))
	store.AppendLog("123", "control.log", []byte("line-2\n"))
	data, err := store.ReadLog("123", "control.log", 7)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "line-2\n" {
		t.Fatalf("unexpected log tail: %q", string(data))
	}
}

func TestProfilesAndPoliciesCanBeMatched(t *testing.T) {
	store := New(t.TempDir())
	if _, err := store.UpsertProfile(RunnerProfile{
		Name:           "default",
		Labels:         []string{"self-hosted", "e2b"},
		TemplateID:     "base",
		MaxConcurrency: 10,
		Enabled:        true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertProfile(RunnerProfile{
		Name:           "large",
		Labels:         []string{"self-hosted", "e2b", "large"},
		TemplateID:     "large",
		MaxConcurrency: 2,
		Priority:       10,
		Enabled:        true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertRepositoryPolicy(RepositoryPolicy{
		RepositoryFullName: "owner/repo",
		ProfileName:        "default",
		Enabled:            true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertRepositoryPolicy(RepositoryPolicy{
		RepositoryFullName: "owner/repo",
		ProfileName:        "large",
		Enabled:            true,
	}); err != nil {
		t.Fatal(err)
	}

	match, err := store.MatchProfile("owner/repo", []string{"self-hosted", "e2b", "large"})
	if err != nil {
		t.Fatal(err)
	}
	if match.Profile == nil || match.Profile.Name != "large" {
		t.Fatalf("expected large profile match, got %#v", match.Profile)
	}
}

func TestMatchProfileReturnsReasonWhenPolicyMissing(t *testing.T) {
	store := New(t.TempDir())
	match, err := store.MatchProfile("owner/repo", []string{"self-hosted", "e2b"})
	if err != nil {
		t.Fatal(err)
	}
	if match.Profile != nil || match.Reason != "profile_not_allowed" {
		t.Fatalf("unexpected match result: %#v", match)
	}
}

func TestDefaultAvailableProfileMatchesWithoutRepositoryPolicy(t *testing.T) {
	store := New(t.TempDir())
	if _, err := store.UpsertProfile(RunnerProfile{
		Name:             "default",
		Labels:           []string{"self-hosted", "e2b"},
		TemplateID:       "base",
		MaxConcurrency:   10,
		Enabled:          true,
		DefaultAvailable: true,
	}); err != nil {
		t.Fatal(err)
	}
	match, err := store.MatchProfile("owner/any-repo", []string{"self-hosted", "e2b"})
	if err != nil {
		t.Fatal(err)
	}
	if match.Profile == nil || match.Profile.Name != "default" {
		t.Fatalf("expected default profile match, got %#v", match.Profile)
	}
}

func TestRunnerGroupPolicyMatchesGroupSpecs(t *testing.T) {
	store := New(t.TempDir())
	if _, err := store.UpsertProfile(RunnerProfile{
		Name:           "small",
		Labels:         []string{"self-hosted", "e2b", "small"},
		TemplateID:     "small-template",
		MaxConcurrency: 10,
		Enabled:        true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertProfile(RunnerProfile{
		Name:           "large",
		Labels:         []string{"self-hosted", "e2b", "large"},
		TemplateID:     "large-template",
		MaxConcurrency: 5,
		Priority:       20,
		Enabled:        true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertRunnerGroup(RunnerGroup{
		Name:      "ubuntu-2404",
		SpecNames: []string{"small", "large"},
		Enabled:   true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertRepositoryPolicy(RepositoryPolicy{
		RepositoryFullName: "owner/repo",
		RunnerGroupName:    "ubuntu-2404",
		Enabled:            true,
	}); err != nil {
		t.Fatal(err)
	}

	match, err := store.MatchProfile("owner/repo", []string{"self-hosted", "e2b", "large"})
	if err != nil {
		t.Fatal(err)
	}
	if match.Profile == nil || match.Profile.Name != "large" {
		t.Fatalf("expected large profile through group policy, got %#v", match.Profile)
	}
}

func TestClaimNextRunnableHonorsLeaseAndRetryWindow(t *testing.T) {
	store := New(t.TempDir())
	now := time.Now().UTC()
	if _, st, err := store.CreateRequest(RunnerRequest{
		ID:              "retryable",
		Source:          "test",
		RequestedLabels: []string{"self-hosted"},
		Labels:          []string{"self-hosted"},
		RunnerName:      "e2b-retryable",
	}, nil); err != nil {
		t.Fatal(err)
	} else {
		st.NextRetryAt = now.Add(-time.Second)
		if err := store.WriteState(st); err != nil {
			t.Fatal(err)
		}
	}

	req, st, claimed, err := store.ClaimNextRunnable("worker-1", now, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !claimed || req.ID != "retryable" || st.LeaseOwner != "worker-1" || st.LastAttemptAt.IsZero() {
		t.Fatalf("unexpected claim result: req=%#v state=%#v claimed=%v", req, st, claimed)
	}

	_, _, claimed, err = store.ClaimNextRunnable("worker-2", now, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if claimed {
		t.Fatal("expected active lease to block second claim")
	}
}

func TestRetryRequestClearsFailureFields(t *testing.T) {
	store := New(t.TempDir())
	_, st, err := store.CreateRequest(RunnerRequest{
		ID:              "retry-me",
		Source:          "test",
		RequestedLabels: []string{"self-hosted"},
		Labels:          []string{"self-hosted"},
		RunnerName:      "e2b-retry-me",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	st.Status = StatusFailed
	st.FailureStage = "sandbox_start"
	st.FailureReason = "backend_server_error"
	st.LastErrorCode = "backend_server_error"
	st.LastErrorMessage = "temporary failure"
	st.LastErrorRetryable = true
	st.NextRetryAt = time.Now().Add(time.Minute).UTC()
	if err := store.WriteState(st); err != nil {
		t.Fatal(err)
	}

	retried, err := store.RetryRequest("retry-me", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if retried.Status != StatusQueued || retried.FailureStage != "" || retried.LastErrorCode != "" || !retried.NextRetryAt.IsZero() {
		t.Fatalf("unexpected retried state: %#v", retried)
	}
}

func TestRetryRequestRejectsActiveState(t *testing.T) {
	store := New(t.TempDir())
	_, st, err := store.CreateRequest(RunnerRequest{
		ID:              "running",
		Source:          "test",
		RequestedLabels: []string{"self-hosted"},
		Labels:          []string{"self-hosted"},
		RunnerName:      "e2b-running",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	st.Status = StatusRunning
	if err := store.WriteState(st); err != nil {
		t.Fatal(err)
	}

	if _, err := store.RetryRequest("running", time.Now().UTC()); !errors.Is(err, ErrRetryNotAllowed) {
		t.Fatalf("expected retry guard, got %v", err)
	}
}

func TestAuditEventsCanBeListed(t *testing.T) {
	store := New(t.TempDir())
	if _, err := store.AppendAuditEvent(AuditEvent{
		Actor:        "admin_api",
		Action:       "runner.retry",
		ResourceType: "runner_request",
		ResourceID:   "retry-me",
		PayloadJSON:  `{"status":"queued"}`,
		CreatedAt:    time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	events, err := store.ListAuditEvents(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Action != "runner.retry" {
		t.Fatalf("unexpected audit events: %#v", events)
	}
}
