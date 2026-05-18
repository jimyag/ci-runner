package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jimyag/e2b-github-runner/internal/config"
	"github.com/jimyag/e2b-github-runner/internal/github"
	"github.com/jimyag/e2b-github-runner/internal/sandboxrunner"
	"github.com/jimyag/e2b-github-runner/internal/state"
)

type fakeSandbox struct {
	started int
	stopped int
}

func (f *fakeSandbox) StartRunner(ctx context.Context, input sandboxrunner.StartInput) (sandboxrunner.StartResult, error) {
	f.started++
	input.OnStdout([]byte("started\n"))
	return sandboxrunner.StartResult{SandboxID: "sb-" + input.RequestID, PID: 42}, nil
}

func (f *fakeSandbox) StopRunner(ctx context.Context, sandboxID string, pid uint32) error {
	f.stopped++
	return nil
}

func TestManualCreateAndDeleteRunner(t *testing.T) {
	ghServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"token":"runner-token","expires_at":"2026-05-18T10:00:00Z"}`))
	}))
	defer ghServer.Close()

	store := state.New(t.TempDir())
	fake := &fakeSandbox{}
	srv := newTestServer(store, ghServer.URL, fake)

	body := bytes.NewBufferString(`{"id":"manual-1"}`)
	req := httptest.NewRequest(http.MethodPost, "/runners", body)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	waitForState(t, store, "manual-1", state.StatusRunning)
	if fake.started != 1 {
		t.Fatalf("expected one sandbox start, got %d", fake.started)
	}

	req = httptest.NewRequest(http.MethodDelete, "/runners/manual-1", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("unexpected delete status: %d body=%s", rec.Code, rec.Body.String())
	}
	if fake.stopped != 1 {
		t.Fatalf("expected one sandbox stop, got %d", fake.stopped)
	}
}

func TestWebhookQueuedIsIdempotent(t *testing.T) {
	ghServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"token":"runner-token","expires_at":"2026-05-18T10:00:00Z"}`))
	}))
	defer ghServer.Close()

	store := state.New(t.TempDir())
	fake := &fakeSandbox{}
	srv := newTestServer(store, ghServer.URL, fake)

	payload := []byte(`{"action":"queued","workflow_job":{"id":1001,"labels":["self-hosted","e2b"]}}`)
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/webhooks/github", bytes.NewReader(payload))
		req.Header.Set("X-GitHub-Event", "workflow_job")
		req.Header.Set("X-Hub-Signature-256", sign("secret", payload))
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusAccepted && rec.Code != http.StatusOK {
			t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
		}
	}
	waitForState(t, store, "1001", state.StatusRunning)
	if fake.started != 1 {
		t.Fatalf("expected one sandbox start, got %d", fake.started)
	}
}

func newTestServer(store *state.Store, ghURL string, fake *fakeSandbox) http.Handler {
	cfg := config.Config{
		StateDir:             "./var/runners",
		GitHubToken:          "gh-token",
		GitHubWebhookSecret:  "secret",
		GitHubOwner:          "o",
		GitHubRepo:           "r",
		SandboxTemplateID:    "base",
		RunnerLabels:         []string{"self-hosted", "e2b"},
		RunnerVersion:        "2.329.0",
		SandboxTimeout:       time.Hour,
		MaxConcurrentRunners: 10,
		GitHubAPIBaseURL:     ghURL,
	}
	gh := github.NewClient(ghURL, "gh-token", "repo", "o", "", "r", http.DefaultClient)
	return New(cfg, store, gh, fake, nil)
}

func waitForState(t *testing.T, store *state.Store, id, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		st, err := store.ReadState(id)
		if err == nil && st.Status == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	st, _ := store.ReadState(id)
	data, _ := json.Marshal(st)
	t.Fatalf("state %s did not become %s: %s", id, want, data)
}

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
