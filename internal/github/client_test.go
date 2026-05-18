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
	var sawAuth bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r/actions/runners/registration-token" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		sawAuth = r.Header.Get("Authorization") == "Bearer gh-token"
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"token":"runner-token","expires_at":"2026-05-18T10:00:00Z"}`))
	}))
	defer ts.Close()

	client := NewClient(ts.URL, "gh-token", "repo", "o", "", "r", ts.Client())
	token, err := client.CreateRegistrationToken(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if token.Token != "runner-token" {
		t.Fatalf("unexpected token: %q", token.Token)
	}
	if !sawAuth {
		t.Fatal("missing authorization header")
	}
}

func TestCreateRegistrationTokenFailure(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	defer ts.Close()

	client := NewClient(ts.URL, "bad-token", "repo", "o", "", "r", ts.Client())
	if _, err := client.CreateRegistrationToken(t.Context()); err == nil {
		t.Fatal("expected error")
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

	client := NewClient(ts.URL, "gh-token", "org", "", "my-org", "", ts.Client())
	token, err := client.CreateRegistrationToken(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if token.Token != "runner-token" {
		t.Fatalf("unexpected token: %q", token.Token)
	}
	if got := client.RunnerURL(); got != "https://github.com/my-org" {
		t.Fatalf("unexpected runner url: %s", got)
	}
}
