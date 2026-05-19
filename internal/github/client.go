package github

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/bradleyfalzon/ghinstallation/v2"
)

type Client struct {
	baseURL string
	token   string
	scope   string
	owner   string
	org     string
	repo    string
	http    *http.Client
}

type AppAuth struct {
	AppID          int64
	InstallationID int64
	PrivateKeyFile string
}

type RegistrationToken struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

func NewClient(baseURL, token, scope, owner, org, repo string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if org == "" {
		org = owner
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		scope:   scope,
		owner:   owner,
		org:     org,
		repo:    repo,
		http:    httpClient,
	}
}

func NewAppClient(baseURL, scope, owner, org, repo string, auth AppAuth, httpClient *http.Client) (*Client, error) {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	privateKey, err := os.ReadFile(auth.PrivateKeyFile)
	if err != nil {
		return nil, err
	}
	baseTransport := httpClient.Transport
	if baseTransport == nil {
		baseTransport = http.DefaultTransport
	}
	transport, err := ghinstallation.New(baseTransport, auth.AppID, auth.InstallationID, privateKey)
	if err != nil {
		return nil, err
	}
	cloned := *httpClient
	cloned.Transport = transport
	return NewClient(baseURL, "", scope, owner, org, repo, &cloned), nil
}

func (c *Client) RunnerURL() string {
	if c.scope == "org" {
		return fmt.Sprintf("https://github.com/%s", c.org)
	}
	return fmt.Sprintf("https://github.com/%s/%s", c.owner, c.repo)
}

func (c *Client) CreateRegistrationToken(ctx context.Context) (RegistrationToken, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/actions/runners/registration-token", c.baseURL, c.owner, c.repo)
	if c.scope == "org" {
		url = fmt.Sprintf("%s/orgs/%s/actions/runners/registration-token", c.baseURL, c.org)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(nil))
	if err != nil {
		return RegistrationToken{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := c.http.Do(req)
	if err != nil {
		return RegistrationToken{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return RegistrationToken{}, fmt.Errorf("github registration token: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var token RegistrationToken
	if err := json.Unmarshal(body, &token); err != nil {
		return RegistrationToken{}, err
	}
	if token.Token == "" {
		return RegistrationToken{}, fmt.Errorf("github registration token response missing token")
	}
	return token, nil
}

func VerifyWebhookSignature(secret string, body []byte, signature string) bool {
	if secret == "" || signature == "" {
		return false
	}
	const prefix = "sha256="
	if !strings.HasPrefix(signature, prefix) {
		return false
	}
	got, err := hex.DecodeString(strings.TrimPrefix(signature, prefix))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	want := mac.Sum(nil)
	return hmac.Equal(got, want)
}

type WorkflowJobEvent struct {
	Action      string      `json:"action"`
	WorkflowJob WorkflowJob `json:"workflow_job"`
	Repository  Repository  `json:"repository"`
	WorkflowRun WorkflowRun `json:"workflow_run"`
}

type WorkflowJob struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	RunnerName string `json:"runner_name"`
	Labels     Labels `json:"labels"`
}

type Repository struct {
	FullName string `json:"full_name"`
	Name     string `json:"name"`
}

type WorkflowRun struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	HeadBranch string `json:"head_branch"`
}

type Labels []string

func (l *Labels) UnmarshalJSON(data []byte) error {
	var stringsOnly []string
	if err := json.Unmarshal(data, &stringsOnly); err == nil {
		*l = stringsOnly
		return nil
	}
	var objects []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(data, &objects); err != nil {
		return err
	}
	out := make([]string, 0, len(objects))
	for _, obj := range objects {
		if obj.Name != "" {
			out = append(out, obj.Name)
		}
	}
	*l = out
	return nil
}

func LabelsMatch(jobLabels, required []string) bool {
	if len(required) == 0 {
		return true
	}
	available := map[string]bool{}
	for _, label := range jobLabels {
		available[strings.ToLower(strings.TrimSpace(label))] = true
	}
	for _, label := range required {
		if !available[strings.ToLower(strings.TrimSpace(label))] {
			return false
		}
	}
	return true
}
