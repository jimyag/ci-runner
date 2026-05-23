package github

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/jimyag/e2b-github-runner/internal/metrics"
)

type Client struct {
	baseURL   string
	scope     string
	owner     string
	org       string
	repo      string
	http      *http.Client
	tokensMu  sync.Mutex
	regTokens map[string]RegistrationToken
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

type Runner struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Busy   bool   `json:"busy"`
}

func NewClient(baseURL, scope, owner, org, repo string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if org == "" {
		org = owner
	}
	return &Client{
		baseURL:   strings.TrimRight(baseURL, "/"),
		scope:     scope,
		owner:     owner,
		org:       org,
		repo:      repo,
		http:      httpClient,
		regTokens: map[string]RegistrationToken{},
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
	transport.BaseURL = strings.TrimRight(baseURL, "/")
	cloned := *httpClient
	cloned.Transport = transport
	return NewClient(baseURL, scope, owner, org, repo, &cloned), nil
}

func NewTokenClient(baseURL, scope, owner, org, repo, token string, httpClient *http.Client) *Client {
	return NewClient(baseURL, scope, owner, org, repo, clientWithTransport(httpClient, bearerTransport{
		token: strings.TrimSpace(token),
	}))
}

func NewBasicAuthClient(baseURL, scope, owner, org, repo, username, password string, httpClient *http.Client) *Client {
	return NewClient(baseURL, scope, owner, org, repo, clientWithTransport(httpClient, basicAuthTransport{
		username: username,
		password: password,
	}))
}

func (c *Client) RunnerURL(repositoryFullName string) (string, error) {
	if c.scope == "org" {
		return fmt.Sprintf("https://github.com/%s", c.org), nil
	}
	repositoryFullName = c.repositoryFullName(repositoryFullName)
	if repositoryFullName == "" {
		return "", fmt.Errorf("repository full name is required for repo runner scope")
	}
	return fmt.Sprintf("https://github.com/%s", repositoryFullName), nil
}

func (c *Client) CreateRegistrationToken(ctx context.Context, repositoryFullName string) (RegistrationToken, error) {
	startedAt := time.Now()
	result := "error"
	defer func() { metrics.RecordGitHubAPI("create_registration_token", result, time.Since(startedAt)) }()
	repositoryFullName = c.repositoryFullName(repositoryFullName)
	if c.scope != "org" && repositoryFullName == "" {
		return RegistrationToken{}, fmt.Errorf("repository full name is required for repo runner scope")
	}
	cacheKey := c.registrationTokenKey(repositoryFullName)
	if token, ok := c.cachedRegistrationToken(cacheKey); ok {
		result = "cache_hit"
		return token, nil
	}
	url := fmt.Sprintf("%s/repos/%s/actions/runners/registration-token", c.baseURL, repositoryFullName)
	if c.scope == "org" {
		url = fmt.Sprintf("%s/orgs/%s/actions/runners/registration-token", c.baseURL, c.org)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(nil))
	if err != nil {
		return RegistrationToken{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
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
	c.storeRegistrationToken(cacheKey, token)
	result = "success"
	return token, nil
}

func (c *Client) ListRunners(ctx context.Context, repositoryFullName string) ([]Runner, error) {
	startedAt := time.Now()
	result := "error"
	defer func() { metrics.RecordGitHubAPI("list_runners", result, time.Since(startedAt)) }()
	repositoryFullName = c.repositoryFullName(repositoryFullName)
	if c.scope != "org" && repositoryFullName == "" {
		return nil, fmt.Errorf("repository full name is required for repo runner scope")
	}
	nextURL := fmt.Sprintf("%s/repos/%s/actions/runners?per_page=100", c.baseURL, repositoryFullName)
	if c.scope == "org" {
		nextURL = fmt.Sprintf("%s/orgs/%s/actions/runners?per_page=100", c.baseURL, c.org)
	}
	var runners []Runner
	for nextURL != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, nextURL, nil)
		if err != nil {
			return nil, err
		}
		setGitHubHeaders(req)
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, err
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		_ = resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, fmt.Errorf("github list runners: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}
		var out struct {
			Runners []Runner `json:"runners"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			return nil, err
		}
		runners = append(runners, out.Runners...)
		nextURL = nextLink(resp.Header.Get("Link"))
	}
	result = "success"
	return runners, nil
}

func (c *Client) RemoveRunner(ctx context.Context, repositoryFullName string, runnerID int64) error {
	startedAt := time.Now()
	result := "error"
	defer func() { metrics.RecordGitHubAPI("remove_runner", result, time.Since(startedAt)) }()
	if runnerID == 0 {
		return fmt.Errorf("runner id is required")
	}
	repositoryFullName = c.repositoryFullName(repositoryFullName)
	if c.scope != "org" && repositoryFullName == "" {
		return fmt.Errorf("repository full name is required for repo runner scope")
	}
	url := fmt.Sprintf("%s/repos/%s/actions/runners/%d", c.baseURL, repositoryFullName, runnerID)
	if c.scope == "org" {
		url = fmt.Sprintf("%s/orgs/%s/actions/runners/%d", c.baseURL, c.org, runnerID)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return err
	}
	setGitHubHeaders(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		result = "not_found"
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("github remove runner: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	result = "success"
	return nil
}

func (c *Client) RemoveRunnerByName(ctx context.Context, repositoryFullName, runnerName string) (bool, error) {
	runnerName = strings.TrimSpace(runnerName)
	if runnerName == "" {
		return false, nil
	}
	runners, err := c.ListRunners(ctx, repositoryFullName)
	if err != nil {
		return false, err
	}
	for _, runner := range runners {
		if runner.Name == runnerName {
			return true, c.RemoveRunner(ctx, repositoryFullName, runner.ID)
		}
	}
	return false, nil
}

func (c *Client) ListWorkflowRunJobs(ctx context.Context, repositoryFullName string, runID int64) ([]WorkflowJob, error) {
	startedAt := time.Now()
	result := "error"
	defer func() { metrics.RecordGitHubAPI("list_workflow_run_jobs", result, time.Since(startedAt)) }()
	repositoryFullName = c.repositoryFullName(repositoryFullName)
	if repositoryFullName == "" {
		return nil, fmt.Errorf("repository full name is required")
	}
	nextURL := fmt.Sprintf("%s/repos/%s/actions/runs/%d/jobs?per_page=100", c.baseURL, repositoryFullName, runID)
	var jobs []WorkflowJob
	for nextURL != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, nextURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

		resp, err := c.http.Do(req)
		if err != nil {
			return nil, err
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		_ = resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, fmt.Errorf("github workflow run jobs: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}
		var out struct {
			Jobs []WorkflowJob `json:"jobs"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			return nil, err
		}
		jobs = append(jobs, out.Jobs...)
		nextURL = nextLink(resp.Header.Get("Link"))
	}
	result = "success"
	return jobs, nil
}

func (c *Client) GetWorkflowJob(ctx context.Context, repositoryFullName string, jobID int64) (WorkflowJob, error) {
	startedAt := time.Now()
	result := "error"
	defer func() { metrics.RecordGitHubAPI("get_workflow_job", result, time.Since(startedAt)) }()
	repositoryFullName = c.repositoryFullName(repositoryFullName)
	if repositoryFullName == "" {
		return WorkflowJob{}, fmt.Errorf("repository full name is required")
	}
	url := fmt.Sprintf("%s/repos/%s/actions/jobs/%d", c.baseURL, repositoryFullName, jobID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return WorkflowJob{}, err
	}
	setGitHubHeaders(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return WorkflowJob{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return WorkflowJob{}, fmt.Errorf("github workflow job: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var job WorkflowJob
	if err := json.Unmarshal(body, &job); err != nil {
		return WorkflowJob{}, err
	}
	result = "success"
	return job, nil
}

func (c *Client) cachedRegistrationToken(key string) (RegistrationToken, bool) {
	c.tokensMu.Lock()
	defer c.tokensMu.Unlock()
	token, ok := c.regTokens[key]
	if !ok {
		return RegistrationToken{}, false
	}
	if token.ExpiresAt.After(time.Now().Add(30 * time.Minute)) {
		return token, true
	}
	delete(c.regTokens, key)
	return RegistrationToken{}, false
}

func (c *Client) storeRegistrationToken(key string, token RegistrationToken) {
	c.tokensMu.Lock()
	defer c.tokensMu.Unlock()
	c.regTokens[key] = token
	for cachedKey, cached := range c.regTokens {
		if cached.ExpiresAt.Before(time.Now()) {
			delete(c.regTokens, cachedKey)
		}
	}
}

func (c *Client) registrationTokenKey(repositoryFullName string) string {
	if c.scope == "org" {
		return "org:" + c.org
	}
	return "repo:" + repositoryFullName
}

func (c *Client) repositoryFullName(repositoryFullName string) string {
	repositoryFullName = strings.TrimSpace(repositoryFullName)
	if repositoryFullName != "" {
		return repositoryFullName
	}
	if c.owner != "" && c.repo != "" {
		return fmt.Sprintf("%s/%s", c.owner, c.repo)
	}
	return ""
}

func nextLink(header string) string {
	for _, part := range strings.Split(header, ",") {
		sections := strings.Split(part, ";")
		if len(sections) < 2 {
			continue
		}
		link := strings.TrimSpace(sections[0])
		if !strings.HasPrefix(link, "<") || !strings.HasSuffix(link, ">") {
			continue
		}
		for _, section := range sections[1:] {
			if strings.TrimSpace(section) == `rel="next"` {
				return strings.TrimSuffix(strings.TrimPrefix(link, "<"), ">")
			}
		}
	}
	return ""
}

func setGitHubHeaders(req *http.Request) {
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
}

type authTransport interface {
	RoundTripper(http.RoundTripper) http.RoundTripper
}

func clientWithTransport(httpClient *http.Client, auth authTransport) *http.Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	baseTransport := httpClient.Transport
	if baseTransport == nil {
		baseTransport = http.DefaultTransport
	}
	cloned := *httpClient
	cloned.Transport = auth.RoundTripper(baseTransport)
	return &cloned
}

type bearerTransport struct {
	token string
}

func (t bearerTransport) RoundTripper(base http.RoundTripper) http.RoundTripper {
	return roundTripFunc(func(req *http.Request) (*http.Response, error) {
		req = cloneRequest(req)
		req.Header.Set("Authorization", "Bearer "+t.token)
		return base.RoundTrip(req)
	})
}

type basicAuthTransport struct {
	username string
	password string
}

func (t basicAuthTransport) RoundTripper(base http.RoundTripper) http.RoundTripper {
	return roundTripFunc(func(req *http.Request) (*http.Response, error) {
		req = cloneRequest(req)
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(t.username+":"+t.password)))
		return base.RoundTrip(req)
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func cloneRequest(req *http.Request) *http.Request {
	cloned := req.Clone(req.Context())
	cloned.Header = req.Header.Clone()
	return cloned
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

type WorkflowRunEvent struct {
	Action      string      `json:"action"`
	WorkflowRun WorkflowRun `json:"workflow_run"`
	Repository  Repository  `json:"repository"`
}

type WorkflowJob struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
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
