package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestVerifyWebhookSignature(t *testing.T) {
	body := []byte(`{"ok":true}`)
	secret := "secret"
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	if !VerifyWebhookSignature(secret, body, sig) {
		t.Fatal("expected valid signature")
	}
	if VerifyWebhookSignature(secret, body, "sha256=deadbeef") {
		t.Fatal("expected invalid signature")
	}
	if VerifyWebhookSignature(secret, body, "") {
		t.Fatal("expected missing signature to fail")
	}
}

func TestLabelsUnmarshalAndMatch(t *testing.T) {
	var event WorkflowJobEvent
	if err := json.Unmarshal([]byte(`{"workflow_job":{"id":1,"labels":[{"name":"self-hosted"},{"name":"e2b"}]}}`), &event); err != nil {
		t.Fatal(err)
	}
	if !LabelsMatch(event.WorkflowJob.Labels, []string{"self-hosted", "e2b"}) {
		t.Fatalf("expected labels to match: %#v", event.WorkflowJob.Labels)
	}
	if LabelsMatch(event.WorkflowJob.Labels, []string{"self-hosted", "linux"}) {
		t.Fatalf("expected labels not to match")
	}
}

func TestCreateRegistrationToken(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r/actions/runners/registration-token" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"token":"runner-token","expires_at":"2026-05-18T10:00:00Z"}`))
	}))
	defer ts.Close()

	client := NewClient(ts.URL, "repo", "o", "", "r", ts.Client())
	token, err := client.CreateRegistrationToken(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	if token.Token != "runner-token" {
		t.Fatalf("unexpected token: %q", token.Token)
	}
}

func TestCreateRegistrationTokenFailure(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	defer ts.Close()

	client := NewClient(ts.URL, "repo", "o", "", "r", ts.Client())
	if _, err := client.CreateRegistrationToken(t.Context(), ""); err == nil {
		t.Fatal("expected error")
	}
}

func TestCreateRegistrationTokenUsesRequestRepository(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/other/repo/actions/runners/registration-token" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"token":"runner-token","expires_at":"2026-05-18T10:00:00Z"}`))
	}))
	defer ts.Close()

	client := NewClient(ts.URL, "repo", "", "", "", ts.Client())
	token, err := client.CreateRegistrationToken(t.Context(), "other/repo")
	if err != nil {
		t.Fatal(err)
	}
	if token.Token != "runner-token" {
		t.Fatalf("unexpected token: %q", token.Token)
	}
	if got, err := client.RunnerURL("other/repo"); err != nil || got != "https://github.com/other/repo" {
		t.Fatalf("unexpected runner url %q err=%v", got, err)
	}
}

func TestCreateOrgRegistrationToken(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/orgs/my-org/actions/runners/registration-token" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"token":"runner-token","expires_at":"2026-05-18T10:00:00Z"}`))
	}))
	defer ts.Close()

	client := NewClient(ts.URL, "org", "", "my-org", "", ts.Client())
	token, err := client.CreateRegistrationToken(t.Context(), "ignored/repo")
	if err != nil {
		t.Fatal(err)
	}
	if token.Token != "runner-token" {
		t.Fatalf("unexpected token: %q", token.Token)
	}
	if got, err := client.RunnerURL("ignored/repo"); err != nil || got != "https://github.com/my-org" {
		t.Fatalf("unexpected runner url: %s", got)
	}
}

func TestListWorkflowRunJobs(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r/actions/runs/123/jobs" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if r.URL.Query().Get("per_page") != "100" {
			t.Fatalf("unexpected query: %s", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"jobs":[{"id":1001,"name":"test","status":"queued","labels":["self-hosted","e2b"]}]}`))
	}))
	defer ts.Close()

	client := NewClient(ts.URL, "repo", "o", "", "r", ts.Client())
	jobs, err := client.ListWorkflowRunJobs(t.Context(), "o/r", 123)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].ID != 1001 || jobs[0].Labels[1] != "e2b" {
		t.Fatalf("unexpected jobs: %#v", jobs)
	}
}

func TestGetWorkflowJob(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r/actions/jobs/1001" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":1001,"name":"test","status":"completed","conclusion":"cancelled","labels":["self-hosted","e2b"]}`))
	}))
	defer ts.Close()

	client := NewClient(ts.URL, "repo", "o", "", "r", ts.Client())
	job, err := client.GetWorkflowJob(t.Context(), "o/r", 1001)
	if err != nil {
		t.Fatal(err)
	}
	if job.ID != 1001 || job.Status != "completed" || job.Conclusion != "cancelled" {
		t.Fatalf("unexpected job: %#v", job)
	}
}
