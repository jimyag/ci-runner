package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jimyag/e2b-github-runner/internal/config"
	"github.com/jimyag/e2b-github-runner/internal/github"
	"github.com/jimyag/e2b-github-runner/internal/metrics"
	"github.com/jimyag/e2b-github-runner/internal/sandboxrunner"
	"github.com/jimyag/e2b-github-runner/internal/state"
)

type Server struct {
	cfg     config.Config
	store   state.Store
	gh      *github.Client
	sandbox sandboxrunner.Service
	logger  *slog.Logger
	mux     *http.ServeMux
	slots   chan struct{}

	admissionMu sync.Mutex
	locks       [64]sync.Mutex
	queueNotify chan struct{}
	startOnce   sync.Once
	workerID    string
	loopCtx     context.Context
	loopCancel  context.CancelFunc
	loopWG      sync.WaitGroup
}

type manualCreateRequest struct {
	ID                 string   `json:"id"`
	RepositoryFullName string   `json:"repository_full_name"`
	ProfileName        string   `json:"runner_spec_name"`
	Labels             []string `json:"labels"`
}

type upsertProfileRequest struct {
	Name             string   `json:"name"`
	Labels           []string `json:"labels"`
	TemplateID       string   `json:"template_id"`
	RunnerGroup      string   `json:"runner_group"`
	MaxConcurrency   int      `json:"max_concurrency"`
	MinIdle          int      `json:"min_idle"`
	Priority         int      `json:"priority"`
	Enabled          *bool    `json:"enabled"`
	DefaultAvailable *bool    `json:"default_available"`
}

type upsertRepositoryPolicyRequest struct {
	RepositoryFullName string `json:"repository_full_name"`
	ProfileName        string `json:"runner_spec_name"`
	RunnerGroupName    string `json:"runner_group_name"`
	Enabled            *bool  `json:"enabled"`
}

type upsertRunnerGroupRequest struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	SpecNames   []string `json:"spec_names"`
	Enabled     *bool    `json:"enabled"`
}

type profileMatchRequest struct {
	RepositoryFullName string   `json:"repository_full_name"`
	Labels             []string `json:"labels"`
}

//go:embed admin/*
var adminAssets embed.FS

const runnerJobStartedMarker = "__runner_job_started__"

func New(cfg config.Config, store state.Store, gh *github.Client, sandbox sandboxrunner.Service, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{
		cfg:         cfg,
		store:       store,
		gh:          gh,
		sandbox:     sandbox,
		logger:      logger,
		mux:         http.NewServeMux(),
		slots:       make(chan struct{}, cfg.MaxConcurrentRunners),
		queueNotify: make(chan struct{}, 1),
	}
	hostname, _ := os.Hostname()
	s.workerID = fmt.Sprintf("%s-%d", hostname, os.Getpid())
	s.loopCtx, s.loopCancel = context.WithCancel(context.Background())
	s.routes()
	s.startBackgroundLoops()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	startedAt := time.Now()
	lw := &loggingResponseWriter{ResponseWriter: w, status: http.StatusOK}
	s.mux.ServeHTTP(lw, r)
	s.logger.Info(
		"http request",
		"method", r.Method,
		"path", r.URL.Path,
		"status", lw.status,
		"bytes", lw.bytes,
		"duration_ms", time.Since(startedAt).Milliseconds(),
		"remote_addr", r.RemoteAddr,
		"github_event", r.Header.Get("X-GitHub-Event"),
		"github_delivery", r.Header.Get("X-GitHub-Delivery"),
	)
}

type loggingResponseWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *loggingResponseWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *loggingResponseWriter) Write(data []byte) (int, error) {
	n, err := w.ResponseWriter.Write(data)
	w.bytes += n
	return n, err
}

func (s *Server) Close() {
	if s.loopCancel != nil {
		s.logger.Info("stopping background loops")
		s.loopCancel()
	}
	s.loopWG.Wait()
	s.logger.Info("background loops stopped")
}

func (s *Server) Recover(ctx context.Context) error {
	states, err := s.store.ListStates()
	if err != nil {
		return err
	}
	s.logger.Info("recovering runner state", "count", len(states))
	for _, st := range states {
		if !isActiveStatus(st.Status) {
			continue
		}
		if err := s.recoverRunner(ctx, st.ID); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /admin", s.handleAdminRedirect)
	s.mux.HandleFunc("GET /admin/", s.handleAdmin)
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("POST /webhooks/github", s.handleGitHubWebhook)
	s.mux.HandleFunc("POST /runner_requests", s.handleCreateRunner)
	s.mux.HandleFunc("GET /runner_requests", s.handleListRunners)
	s.mux.HandleFunc("GET /runner_requests/{id}", s.handleGetRunner)
	s.mux.HandleFunc("POST /runner_requests/{id}/retry", s.handleRetryRunner)
	s.mux.HandleFunc("GET /runner_requests/{id}/logs/{name}", s.handleGetRunnerLog)
	s.mux.HandleFunc("DELETE /runner_requests/{id}", s.handleDeleteRunner)
	s.mux.HandleFunc("GET /audit-events", s.handleListAuditEvents)
	s.mux.HandleFunc("GET /runner_specs", s.handleListProfiles)
	s.mux.HandleFunc("POST /runner_specs", s.handleCreateProfile)
	s.mux.HandleFunc("POST /runner_specs/match", s.handleMatchProfile)
	s.mux.HandleFunc("GET /runner_specs/{name}", s.handleGetProfile)
	s.mux.HandleFunc("PATCH /runner_specs/{name}", s.handlePatchProfile)
	s.mux.HandleFunc("DELETE /runner_specs/{name}", s.handleDeleteProfile)
	s.mux.HandleFunc("GET /runner_groups", s.handleListRunnerGroups)
	s.mux.HandleFunc("POST /runner_groups", s.handleCreateRunnerGroup)
	s.mux.HandleFunc("GET /runner_groups/{name}", s.handleGetRunnerGroup)
	s.mux.HandleFunc("PATCH /runner_groups/{name}", s.handlePatchRunnerGroup)
	s.mux.HandleFunc("DELETE /runner_groups/{name}", s.handleDeleteRunnerGroup)
	s.mux.HandleFunc("GET /runner_policies", s.handleListRepositoryPolicies)
	s.mux.HandleFunc("POST /runner_policies", s.handleCreateRepositoryPolicy)
	s.mux.HandleFunc("PATCH /runner_policies/{id}", s.handlePatchRepositoryPolicy)
	s.mux.HandleFunc("DELETE /runner_policies/{id}", s.handleDeleteRepositoryPolicy)
	s.mux.HandleFunc("GET /diagnostics/pprof", s.handleDiagnosticsPprof)
	s.mux.HandleFunc("GET /diagnostics/vars", s.handleDiagnosticsVars)
}

func (s *Server) startBackgroundLoops() {
	s.startOnce.Do(func() {
		s.logger.Info("starting background loops", "worker_id", s.workerID, "max_concurrent_runners", s.cfg.MaxConcurrentRunners)
		s.loopWG.Add(3)
		go s.workerLoop(s.loopCtx)
		go s.sweeperLoop(s.loopCtx)
		go s.reconcilerLoop(s.loopCtx)
	})
}

func (s *Server) workerLoop(ctx context.Context) {
	defer s.loopWG.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.queueNotify:
		case <-time.After(500 * time.Millisecond):
		}
		s.processQueuedRequests(ctx)
	}
}

func (s *Server) sweeperLoop(ctx context.Context) {
	defer s.loopWG.Done()
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sweepOnce()
		}
	}
}

func (s *Server) reconcilerLoop(ctx context.Context) {
	defer s.loopWG.Done()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.reconcileOnce(ctx)
		}
	}
}

func (s *Server) processQueuedRequests(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case s.slots <- struct{}{}:
		default:
			return
		}
		req, _, claimed, err := s.store.ClaimNextRunnable(s.workerID, time.Now().UTC(), s.cfg.WorkerLeaseTTL)
		if err != nil {
			<-s.slots
			s.logger.Error("claim queued runner", "error", err)
			return
		}
		if !claimed {
			<-s.slots
			return
		}
		s.logger.Info("claimed queued runner request", "id", req.ID, "worker_id", s.workerID, "repository", req.RepositoryFullName, "profile", req.ProfileName)
		go func(id string) {
			defer func() { <-s.slots }()
			s.startRunner(ctx, id, s.workerID)
		}(req.ID)
	}
}

func (s *Server) refreshMetrics() {
	profiles, err := s.store.ListProfiles()
	if err != nil {
		s.logger.Error("refresh metrics list profiles", "error", err)
		return
	}
	states, err := s.store.ListStates()
	if err != nil {
		s.logger.Error("refresh metrics list states", "error", err)
		return
	}
	metrics.Refresh(profiles, states)
}

func (s *Server) sweepOnce() {
	states, err := s.store.ListStates()
	if err != nil {
		s.logger.Error("list states for sweeper", "error", err)
		return
	}
	now := time.Now().UTC()
	for _, st := range states {
		switch st.Status {
		case state.StatusCreating:
			if !st.CreatingAt.IsZero() && now.Sub(st.CreatingAt) > s.cfg.SandboxCreateTimeout {
				s.logger.Info("sweeper detected create timeout", "id", st.ID, "creating_at", st.CreatingAt)
				s.retryOrFail(st, "sweeper_create_timeout", fmt.Errorf("runner create timed out"))
			}
		case state.StatusRunning:
			if s.shouldStopIdleRunner(st, now) {
				s.logger.Info("sweeper stopping idle runner", "id", st.ID, "sandbox_id", st.SandboxID, "running_at", st.RunningAt, "idle_timeout", s.cfg.RunnerIdleTimeout)
				s.store.AppendLog(st.ID, "control.log", []byte("sweeper stopping idle runner that never accepted a job\n"))
				s.stopIfExists(context.Background(), st.ID, github.WorkflowJob{})
				continue
			}
			if !st.RunningAt.IsZero() && now.Sub(st.RunningAt) > s.cfg.SandboxTimeout {
				s.logger.Info("sweeper stopping timed out runner", "id", st.ID, "sandbox_id", st.SandboxID, "running_at", st.RunningAt)
				s.stopIfExists(context.Background(), st.ID, github.WorkflowJob{})
			}
		case state.StatusStopping:
			if !st.NextRetryAt.IsZero() && now.Before(st.NextRetryAt) {
				continue
			}
			if !st.StoppingAt.IsZero() && now.Sub(st.StoppingAt) > s.cfg.SandboxStopTimeout {
				s.logger.Info("sweeper retrying timed out stop", "id", st.ID, "sandbox_id", st.SandboxID, "stopping_at", st.StoppingAt)
				s.stopIfExists(context.Background(), st.ID, github.WorkflowJob{})
			}
		}
	}
}

func (s *Server) shouldStopIdleRunner(st state.RunnerState, now time.Time) bool {
	if s.cfg.RunnerIdleTimeout <= 0 || st.RunningAt.IsZero() {
		return false
	}
	if st.AssignedJobID != 0 || st.AssignedJobName == runnerJobStartedMarker {
		return false
	}
	return now.Sub(st.RunningAt) > s.cfg.RunnerIdleTimeout
}

func (s *Server) reconcileOnce(ctx context.Context) {
	s.processQueuedRequests(ctx)
	states, err := s.store.ListStates()
	if err != nil {
		s.logger.Error("list states for reconciler", "error", err)
		return
	}
	for _, st := range states {
		switch st.Status {
		case state.StatusQueued:
			s.signalQueue()
		case state.StatusStopping:
			if !st.NextRetryAt.IsZero() && time.Now().UTC().Before(st.NextRetryAt) {
				continue
			}
			if !st.StoppingAt.IsZero() && time.Since(st.StoppingAt) > s.cfg.SandboxStopTimeout {
				s.logger.Info("reconciler retrying timed out stop", "id", st.ID, "sandbox_id", st.SandboxID, "stopping_at", st.StoppingAt)
				s.stopIfExists(context.Background(), st.ID, github.WorkflowJob{})
			}
		}
	}
}

func (s *Server) handleAdminRedirect(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/admin/", http.StatusMovedPermanently)
}

func (s *Server) handleAdmin(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/admin/")
	if name == "" {
		name = "index.html"
	}
	name = path.Clean("/" + name)
	if strings.HasPrefix(name, "/..") {
		http.NotFound(w, r)
		return
	}
	data, err := adminAssets.ReadFile("admin" + name)
	if err != nil {
		if path.Ext(name) != "" {
			http.NotFound(w, r)
			return
		}
		data, err = adminAssets.ReadFile("admin/index.html")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		name = "/index.html"
	}
	if contentType := mime.TypeByExtension(path.Ext(name)); contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleGitHubWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 10<<20))
	if err != nil {
		s.logger.Warn("github webhook read body failed", "event", r.Header.Get("X-GitHub-Event"), "delivery", r.Header.Get("X-GitHub-Delivery"), "error", err)
		writeError(w, http.StatusBadRequest, "read body")
		return
	}
	if !github.VerifyWebhookSignature(s.cfg.GitHubWebhookSecret, body, r.Header.Get("X-Hub-Signature-256")) {
		s.logger.Warn("github webhook signature rejected", "event", r.Header.Get("X-GitHub-Event"), "delivery", r.Header.Get("X-GitHub-Delivery"))
		writeError(w, http.StatusUnauthorized, "invalid signature")
		return
	}
	eventName := r.Header.Get("X-GitHub-Event")
	s.logger.Info("github webhook received", "event", eventName, "delivery", r.Header.Get("X-GitHub-Delivery"), "bytes", len(body))
	if eventName == "workflow_run" {
		s.handleWorkflowRunWebhook(w, r, body)
		return
	}
	if eventName != "workflow_job" {
		s.logger.Info("github webhook ignored", "event", eventName, "delivery", r.Header.Get("X-GitHub-Delivery"))
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "ignored"})
		return
	}
	var event github.WorkflowJobEvent
	if err := json.Unmarshal(body, &event); err != nil {
		s.logger.Warn("github workflow_job payload rejected", "delivery", r.Header.Get("X-GitHub-Delivery"), "error", err)
		writeError(w, http.StatusBadRequest, "invalid workflow_job payload")
		return
	}
	id := strconv.FormatInt(event.WorkflowJob.ID, 10)
	s.logger.Info("workflow_job webhook parsed", "action", event.Action, "job_id", id, "job_name", event.WorkflowJob.Name, "repository", event.Repository.FullName, "runner_name", event.WorkflowJob.RunnerName, "labels", []string(event.WorkflowJob.Labels))
	switch event.Action {
	case "queued":
		match, err := s.store.MatchProfile(event.Repository.FullName, event.WorkflowJob.Labels)
		if err != nil {
			s.logger.Error("match workflow job profile", "job_id", id, "repository", event.Repository.FullName, "labels", []string(event.WorkflowJob.Labels), "error", err)
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		req := state.RunnerRequest{
			ID:                 id,
			Source:             "github_webhook",
			JobID:              event.WorkflowJob.ID,
			RepositoryFullName: event.Repository.FullName,
			RequestedLabels:    append([]string(nil), event.WorkflowJob.Labels...),
			Labels:             []string(event.WorkflowJob.Labels),
			RunnerName:         "e2b-" + id,
		}
		if match.Profile == nil {
			s.logger.Info("workflow_job admission rejected", "job_id", id, "repository", event.Repository.FullName, "labels", []string(event.WorkflowJob.Labels), "reason", match.Reason)
			st, err := s.rejectAdmission(req, body, match.Reason)
			if err != nil {
				s.logger.Error("write workflow_job admission rejection", "job_id", id, "error", err)
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSON(w, http.StatusAccepted, st)
			return
		}
		req.ProfileName = match.Profile.Name
		req.RunnerGroup = match.Profile.RunnerGroup
		req.Labels = append([]string(nil), match.Profile.Labels...)
		s.logger.Info("workflow_job matched profile", "job_id", id, "repository", event.Repository.FullName, "profile", match.Profile.Name, "runner_group", match.Profile.RunnerGroup, "labels", req.Labels)
		metrics.RecordWorkflowQueued(event.Repository.FullName, event.WorkflowRun.Name, event.WorkflowJob.Name, match.Profile.Name)
		s.createAndStart(w, r, req, body)
	case "completed":
		stopID, reason := s.completedWorkflowJobStopID(event.WorkflowJob)
		if stopID == "" {
			s.logger.Info("completed workflow job has no managed runner", "job_id", id, "runner_name", event.WorkflowJob.RunnerName)
			writeJSON(w, http.StatusAccepted, map[string]string{"status": "ignored"})
			return
		}
		st, err := s.stopRunner(context.Background(), stopID, event.WorkflowJob)
		if err != nil {
			s.logger.Error("stop runner", "id", stopID, "error", err)
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		metrics.RecordWorkflowCompleted(st.RepositoryFullName, event.WorkflowRun.Name, event.WorkflowJob.Name, st.ProfileName, "completed")
		s.logger.Info("workflow_job completed handled", "job_id", id, "request_id", stopID, "repository", st.RepositoryFullName, "status", st.Status, "matched_by", reason)
		writeJSON(w, http.StatusAccepted, st)
	default:
		s.logger.Info("workflow_job webhook ignored", "action", event.Action, "job_id", id, "repository", event.Repository.FullName)
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "ignored"})
	}
}

func (s *Server) completedWorkflowJobStopID(job github.WorkflowJob) (string, string) {
	if strings.HasPrefix(job.RunnerName, "e2b-") {
		return strings.TrimPrefix(job.RunnerName, "e2b-"), "runner_name"
	}
	if job.ID == 0 {
		return "", ""
	}
	id := strconv.FormatInt(job.ID, 10)
	if _, err := s.store.ReadState(id); err == nil {
		return id, "workflow_job_id"
	}
	return "", ""
}

func (s *Server) handleWorkflowRunWebhook(w http.ResponseWriter, r *http.Request, body []byte) {
	var event github.WorkflowRunEvent
	if err := json.Unmarshal(body, &event); err != nil {
		s.logger.Warn("github workflow_run payload rejected", "delivery", r.Header.Get("X-GitHub-Delivery"), "error", err)
		writeError(w, http.StatusBadRequest, "invalid workflow_run payload")
		return
	}
	s.logger.Info("workflow_run webhook parsed", "action", event.Action, "run_id", event.WorkflowRun.ID, "workflow", event.WorkflowRun.Name, "repository", event.Repository.FullName)
	switch event.Action {
	case "requested", "in_progress":
	default:
		s.logger.Info("workflow_run webhook ignored", "action", event.Action, "run_id", event.WorkflowRun.ID, "repository", event.Repository.FullName, "delivery", r.Header.Get("X-GitHub-Delivery"))
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "ignored"})
		return
	}
	jobs, err := s.gh.ListWorkflowRunJobs(r.Context(), event.Repository.FullName, event.WorkflowRun.ID)
	if err != nil {
		s.logger.Error("list workflow run jobs", "run_id", event.WorkflowRun.ID, "repository", event.Repository.FullName, "error", err)
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	created := 0
	existing := 0
	skipped := 0
	for _, job := range jobs {
		if job.ID == 0 || job.Status != "queued" {
			s.logger.Info("workflow_run job skipped", "run_id", event.WorkflowRun.ID, "job_id", job.ID, "job_name", job.Name, "status", job.Status, "repository", event.Repository.FullName)
			skipped++
			continue
		}
		st, wasCreated, err := s.enqueueWorkflowJob(event.Repository.FullName, event.WorkflowRun.Name, job, body)
		if err != nil {
			s.logger.Error("enqueue workflow_run job", "run_id", event.WorkflowRun.ID, "job_id", job.ID, "error", err)
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if wasCreated {
			created++
		} else if st.Status == state.StatusFailed && st.FailureStage == "admission" {
			skipped++
		} else {
			existing++
		}
	}
	s.logger.Info("workflow_run webhook reconciled jobs", "action", event.Action, "run_id", event.WorkflowRun.ID, "repository", event.Repository.FullName, "created", created, "existing", existing, "skipped", skipped)
	writeJSON(w, http.StatusAccepted, map[string]int{"created": created, "existing": existing, "skipped": skipped})
}

func (s *Server) handleCreateRunner(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminAuth(w, r) {
		return
	}
	var input manualCreateRequest
	if r.Body != nil {
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&input); err != nil && !errors.Is(err, io.EOF) {
			s.logger.Warn("manual runner payload rejected", "error", err)
			writeError(w, http.StatusBadRequest, "invalid runner payload")
			return
		}
	}
	id := input.ID
	if id == "" {
		id = newID()
	}
	labels := input.Labels
	requestedLabels := append([]string(nil), labels...)
	repositoryFullName := strings.TrimSpace(input.RepositoryFullName)
	if repositoryFullName == "" {
		repositoryFullName = s.cfg.DefaultRepositoryPattern()
	}
	if strings.TrimSpace(repositoryFullName) == "" || strings.Contains(repositoryFullName, "*") {
		s.logger.Info("manual runner repository missing", "id", id, "repository", repositoryFullName)
		writeError(w, http.StatusBadRequest, "repository_full_name is required for manual runner creation")
		return
	}
	profileName := strings.TrimSpace(input.ProfileName)
	runnerGroup := ""
	if profileName == "" {
		if len(labels) == 0 {
			labels = s.cfg.RunnerLabels
		}
		match, err := s.store.MatchProfile(repositoryFullName, labels)
		if err != nil {
			s.logger.Error("match manual runner profile", "id", id, "repository", repositoryFullName, "labels", labels, "error", err)
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if match.Profile == nil {
			s.logger.Info("manual runner admission rejected", "id", id, "repository", repositoryFullName, "labels", labels, "reason", match.Reason)
			writeError(w, http.StatusBadRequest, "no matching profile")
			return
		}
		profileName = match.Profile.Name
		runnerGroup = match.Profile.RunnerGroup
		labels = append([]string(nil), match.Profile.Labels...)
	} else {
		profile, err := s.store.GetProfile(profileName)
		if err != nil {
			s.logger.Info("manual runner profile not found", "id", id, "repository", repositoryFullName, "profile", profileName)
			writeError(w, http.StatusBadRequest, "profile not found")
			return
		}
		if err := s.ensureRepositoryAllowsProfile(repositoryFullName, profile, labels); err != nil {
			s.logger.Info("manual runner repository policy rejected", "id", id, "repository", repositoryFullName, "profile", profileName, "labels", labels, "error", err)
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		runnerGroup = profile.RunnerGroup
		if len(labels) == 0 {
			labels = append([]string(nil), profile.Labels...)
		}
		if len(requestedLabels) == 0 {
			requestedLabels = append([]string(nil), labels...)
		}
	}
	req := state.RunnerRequest{
		ID:                 id,
		Source:             "manual_api",
		RepositoryFullName: repositoryFullName,
		Labels:             labels,
		RequestedLabels:    requestedLabels,
		ProfileName:        profileName,
		RunnerGroup:        runnerGroup,
		RunnerName:         "e2b-" + id,
	}
	s.logger.Info("manual runner create requested", "id", id, "repository", repositoryFullName, "profile", profileName, "labels", labels)
	s.createAndStart(w, r, req, nil)
}

func (s *Server) handleListRunners(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminAuth(w, r) {
		return
	}
	s.refreshMetrics()
	states, err := s.store.ListStates()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, states)
}

func (s *Server) handleGetRunner(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminAuth(w, r) {
		return
	}
	st, err := s.store.ReadState(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "runner not found")
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleRetryRunner(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminAuth(w, r) {
		return
	}
	st, err := s.store.RetryRequest(r.PathValue("id"), time.Now().UTC())
	if err != nil {
		if errors.Is(err, state.ErrRetryNotAllowed) {
			s.logger.Info("runner retry rejected", "id", r.PathValue("id"), "error", err)
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		s.logger.Error("retry runner request", "id", r.PathValue("id"), "error", err)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.store.AppendLog(st.ID, "control.log", []byte("runner request manually requeued\n"))
	s.logger.Info("runner request manually requeued", "id", st.ID, "repository", st.RepositoryFullName, "profile", st.ProfileName)
	s.recordAudit("admin_api", "runner.retry", "runner_request", st.ID, map[string]any{
		"status":           st.Status,
		"repository":       st.RepositoryFullName,
		"runner_spec_name": st.ProfileName,
		"requested_labels": st.RequestedLabels,
	})
	s.refreshMetrics()
	writeJSON(w, http.StatusAccepted, st)
}

func (s *Server) handleGetRunnerLog(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminAuth(w, r) {
		return
	}
	name := r.PathValue("name")
	switch name {
	case "control.log", "stdout.log", "stderr.log":
	default:
		writeError(w, http.StatusBadRequest, "unsupported log name")
		return
	}
	data, err := s.store.ReadLog(r.PathValue("id"), name, 256<<10)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusOK)
			return
		}
		writeError(w, http.StatusNotFound, "log not found")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

func (s *Server) handleDeleteRunner(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminAuth(w, r) {
		return
	}
	id := r.PathValue("id")
	st, err := s.stopRunner(context.Background(), id, github.WorkflowJob{})
	if err != nil {
		s.logger.Error("delete runner request failed", "id", id, "error", err)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.logger.Info("delete runner request handled", "id", id, "status", st.Status, "sandbox_id", st.SandboxID)
	s.recordAudit("admin_api", "runner.stop", "runner_request", id, map[string]any{
		"status": st.Status,
	})
	writeJSON(w, http.StatusAccepted, st)
}

func (s *Server) handleListAuditEvents(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminAuth(w, r) {
		return
	}
	events, err := s.store.ListAuditEvents(100)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, events)
}

func (s *Server) handleListProfiles(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminAuth(w, r) {
		return
	}
	s.refreshMetrics()
	profiles, err := s.store.ListProfiles()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, profiles)
}

func (s *Server) handleCreateProfile(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminAuth(w, r) {
		return
	}
	var input upsertProfileRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid profile payload")
		return
	}
	enabled := true
	if input.Enabled != nil {
		enabled = *input.Enabled
	}
	profile, err := s.store.UpsertProfile(state.RunnerProfile{
		Name:             input.Name,
		Labels:           input.Labels,
		TemplateID:       input.TemplateID,
		RunnerGroup:      input.RunnerGroup,
		MaxConcurrency:   input.MaxConcurrency,
		MinIdle:          input.MinIdle,
		Priority:         input.Priority,
		Enabled:          enabled,
		DefaultAvailable: input.DefaultAvailable != nil && *input.DefaultAvailable,
	})
	if err != nil {
		s.logger.Info("profile create rejected", "name", input.Name, "error", err)
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.logger.Info("profile created", "name", profile.Name, "labels", profile.Labels, "template_id", profile.TemplateID, "max_concurrency", profile.MaxConcurrency, "enabled", profile.Enabled)
	s.recordAudit("admin_api", "profile.create", "runner_profile", profile.Name, profile)
	s.refreshMetrics()
	writeJSON(w, http.StatusCreated, profile)
}

func (s *Server) handleGetProfile(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminAuth(w, r) {
		return
	}
	profile, err := s.store.GetProfile(r.PathValue("name"))
	if err != nil {
		writeError(w, http.StatusNotFound, "profile not found")
		return
	}
	writeJSON(w, http.StatusOK, profile)
}

func (s *Server) handlePatchProfile(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminAuth(w, r) {
		return
	}
	current, err := s.store.GetProfile(r.PathValue("name"))
	if err != nil {
		writeError(w, http.StatusNotFound, "profile not found")
		return
	}
	var input upsertProfileRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid profile payload")
		return
	}
	if len(input.Labels) > 0 {
		current.Labels = input.Labels
	}
	if input.TemplateID != "" {
		current.TemplateID = input.TemplateID
	}
	if input.RunnerGroup != "" {
		current.RunnerGroup = input.RunnerGroup
	}
	if input.MaxConcurrency > 0 {
		current.MaxConcurrency = input.MaxConcurrency
	}
	current.MinIdle = input.MinIdle
	current.Priority = input.Priority
	if input.Enabled != nil {
		current.Enabled = *input.Enabled
	}
	if input.DefaultAvailable != nil {
		current.DefaultAvailable = *input.DefaultAvailable
	}
	profile, err := s.store.UpsertProfile(current)
	if err != nil {
		s.logger.Info("profile update rejected", "name", current.Name, "error", err)
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.logger.Info("profile updated", "name", profile.Name, "labels", profile.Labels, "template_id", profile.TemplateID, "max_concurrency", profile.MaxConcurrency, "enabled", profile.Enabled)
	s.recordAudit("admin_api", "profile.update", "runner_profile", profile.Name, profile)
	s.refreshMetrics()
	writeJSON(w, http.StatusOK, profile)
}

func (s *Server) handleDeleteProfile(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminAuth(w, r) {
		return
	}
	if err := s.store.DeleteProfile(r.PathValue("name")); err != nil {
		s.logger.Error("delete profile", "name", r.PathValue("name"), "error", err)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.logger.Info("profile deleted", "name", r.PathValue("name"))
	s.recordAudit("admin_api", "profile.delete", "runner_profile", r.PathValue("name"), map[string]any{"status": "deleted"})
	s.refreshMetrics()
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleListRunnerGroups(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminAuth(w, r) {
		return
	}
	groups, err := s.store.ListRunnerGroups()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, groups)
}

func (s *Server) handleCreateRunnerGroup(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminAuth(w, r) {
		return
	}
	var input upsertRunnerGroupRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid runner group payload")
		return
	}
	enabled := true
	if input.Enabled != nil {
		enabled = *input.Enabled
	}
	group, err := s.store.UpsertRunnerGroup(state.RunnerGroup{
		Name:        input.Name,
		Description: input.Description,
		SpecNames:   input.SpecNames,
		Enabled:     enabled,
	})
	if err != nil {
		s.logger.Info("runner group create rejected", "name", input.Name, "error", err)
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.logger.Info("runner group created", "name", group.Name, "specs", group.SpecNames, "enabled", group.Enabled)
	s.recordAudit("admin_api", "runner_group.create", "runner_group", group.Name, group)
	writeJSON(w, http.StatusCreated, group)
}

func (s *Server) handleGetRunnerGroup(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminAuth(w, r) {
		return
	}
	group, err := s.store.GetRunnerGroup(r.PathValue("name"))
	if err != nil {
		writeError(w, http.StatusNotFound, "runner group not found")
		return
	}
	writeJSON(w, http.StatusOK, group)
}

func (s *Server) handlePatchRunnerGroup(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminAuth(w, r) {
		return
	}
	current, err := s.store.GetRunnerGroup(r.PathValue("name"))
	if err != nil {
		writeError(w, http.StatusNotFound, "runner group not found")
		return
	}
	var input upsertRunnerGroupRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid runner group payload")
		return
	}
	if input.Description != "" {
		current.Description = input.Description
	}
	if input.SpecNames != nil {
		current.SpecNames = input.SpecNames
	}
	if input.Enabled != nil {
		current.Enabled = *input.Enabled
	}
	group, err := s.store.UpsertRunnerGroup(current)
	if err != nil {
		s.logger.Info("runner group update rejected", "name", current.Name, "error", err)
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.logger.Info("runner group updated", "name", group.Name, "specs", group.SpecNames, "enabled", group.Enabled)
	s.recordAudit("admin_api", "runner_group.update", "runner_group", group.Name, group)
	writeJSON(w, http.StatusOK, group)
}

func (s *Server) handleDeleteRunnerGroup(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminAuth(w, r) {
		return
	}
	name := r.PathValue("name")
	if err := s.store.DeleteRunnerGroup(name); err != nil {
		s.logger.Error("delete runner group", "name", name, "error", err)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.logger.Info("runner group deleted", "name", name)
	s.recordAudit("admin_api", "runner_group.delete", "runner_group", name, map[string]any{"status": "deleted"})
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleListRepositoryPolicies(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminAuth(w, r) {
		return
	}
	s.refreshMetrics()
	policies, err := s.store.ListRepositoryPolicies()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, policies)
}

func (s *Server) handleCreateRepositoryPolicy(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminAuth(w, r) {
		return
	}
	var input upsertRepositoryPolicyRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid repository policy payload")
		return
	}
	enabled := true
	if input.Enabled != nil {
		enabled = *input.Enabled
	}
	policy, err := s.store.UpsertRepositoryPolicy(state.RepositoryPolicy{
		RepositoryFullName: input.RepositoryFullName,
		ProfileName:        input.ProfileName,
		RunnerGroupName:    input.RunnerGroupName,
		Enabled:            enabled,
	})
	if err != nil {
		s.logger.Info("repository policy create rejected", "repository", input.RepositoryFullName, "profile", input.ProfileName, "runner_group", input.RunnerGroupName, "error", err)
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.logger.Info("repository policy created", "id", policy.ID, "repository", policy.RepositoryFullName, "profile", policy.ProfileName, "runner_group", policy.RunnerGroupName, "enabled", policy.Enabled)
	s.recordAudit("admin_api", "repository_policy.create", "repository_policy", strconv.FormatInt(policy.ID, 10), policy)
	s.refreshMetrics()
	writeJSON(w, http.StatusCreated, policy)
}

func (s *Server) handlePatchRepositoryPolicy(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminAuth(w, r) {
		return
	}
	policies, err := s.store.ListRepositoryPolicies()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var existing *state.RepositoryPolicy
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid policy id")
		return
	}
	for i := range policies {
		if policies[i].ID == id {
			existing = &policies[i]
			break
		}
	}
	if existing == nil {
		writeError(w, http.StatusNotFound, "repository policy not found")
		return
	}
	var input upsertRepositoryPolicyRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid repository policy payload")
		return
	}
	if input.RepositoryFullName != "" {
		existing.RepositoryFullName = input.RepositoryFullName
	}
	if input.ProfileName != "" {
		existing.ProfileName = input.ProfileName
		existing.RunnerGroupName = ""
	}
	if input.RunnerGroupName != "" {
		existing.RunnerGroupName = input.RunnerGroupName
		existing.ProfileName = ""
	}
	if input.Enabled != nil {
		existing.Enabled = *input.Enabled
	}
	policy, err := s.store.UpsertRepositoryPolicy(*existing)
	if err != nil {
		s.logger.Info("repository policy update rejected", "id", id, "repository", existing.RepositoryFullName, "profile", existing.ProfileName, "runner_group", existing.RunnerGroupName, "error", err)
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.logger.Info("repository policy updated", "id", policy.ID, "repository", policy.RepositoryFullName, "profile", policy.ProfileName, "runner_group", policy.RunnerGroupName, "enabled", policy.Enabled)
	s.recordAudit("admin_api", "repository_policy.update", "repository_policy", strconv.FormatInt(policy.ID, 10), policy)
	s.refreshMetrics()
	writeJSON(w, http.StatusOK, policy)
}

func (s *Server) handleDeleteRepositoryPolicy(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminAuth(w, r) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid policy id")
		return
	}
	if err := s.store.DeleteRepositoryPolicy(id); err != nil {
		s.logger.Error("delete repository policy", "id", id, "error", err)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.logger.Info("repository policy deleted", "id", id)
	s.recordAudit("admin_api", "repository_policy.delete", "repository_policy", strconv.FormatInt(id, 10), map[string]any{"status": "deleted"})
	s.refreshMetrics()
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleMatchProfile(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminAuth(w, r) {
		return
	}
	var input profileMatchRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid match payload")
		return
	}
	match, err := s.store.MatchProfile(input.RepositoryFullName, input.Labels)
	if err != nil {
		s.logger.Error("match profile request failed", "repository", input.RepositoryFullName, "labels", input.Labels, "error", err)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if match.Profile == nil {
		s.logger.Info("match profile result", "repository", input.RepositoryFullName, "labels", input.Labels, "matched", false, "reason", match.Reason)
	} else {
		s.logger.Info("match profile result", "repository", input.RepositoryFullName, "labels", input.Labels, "matched", true, "profile", match.Profile.Name)
	}
	writeJSON(w, http.StatusOK, match)
}

func (s *Server) handleDiagnosticsPprof(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminAuth(w, r) {
		return
	}
	type artifact struct {
		Address     string `json:"address"`
		AddressFile string `json:"address_file"`
		DumpScript  string `json:"dump_script"`
	}
	addresses, scripts := discoverPprofArtifacts()
	states, _ := s.store.ListStates()
	failures := make([]state.RunnerState, 0, 5)
	for _, st := range states {
		if st.Status == state.StatusFailed {
			failures = append(failures, st)
			if len(failures) == 5 {
				break
			}
		}
	}
	out := make([]artifact, 0, len(addresses))
	for i := range addresses {
		item := artifact{Address: addresses[i].Address, AddressFile: addresses[i].Path}
		if i < len(scripts) {
			item.DumpScript = scripts[i]
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"pprof": out,
		"state": map[string]any{
			"backend":  s.cfg.StateBackend,
			"database": s.cfg.StateDatabaseURL,
		},
		"github": map[string]any{
			"auth_mode":       s.cfg.GitHubAuthMode(),
			"installation_id": s.cfg.GitHubAppInstallationID,
			"api_base_url":    s.cfg.GitHubAPIBaseURL,
		},
		"sandbox": map[string]any{
			"api_url": s.cfg.E2BAPIURL,
			"domain":  s.cfg.E2BDomain,
		},
		"recent_failures": failures,
	})
}

func (s *Server) handleDiagnosticsVars(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminAuth(w, r) {
		return
	}
	addresses, _ := discoverPprofArtifacts()
	if len(addresses) == 0 {
		writeError(w, http.StatusNotFound, "pprof endpoint not discovered")
		return
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(strings.TrimRight(addresses[0].Address, "/") + "/debug/vars")
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		writeError(w, http.StatusBadGateway, resp.Status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.Copy(w, resp.Body)
}

func (s *Server) createAndStart(w http.ResponseWriter, r *http.Request, req state.RunnerRequest, payload []byte) {
	st, created, err := s.enqueueRunnerRequest(req, payload)
	if err != nil {
		if errors.Is(err, errRunnerConcurrencyLimit) || errors.Is(err, errProfileConcurrencyLimit) {
			s.logger.Info("runner request rejected by concurrency limit", "id", req.ID, "repository", req.RepositoryFullName, "profile", req.ProfileName, "error", err)
			writeError(w, http.StatusTooManyRequests, err.Error())
			return
		}
		s.logger.Error("enqueue runner request failed", "id", req.ID, "repository", req.RepositoryFullName, "profile", req.ProfileName, "error", err)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if created {
		s.logger.Info("runner request accepted", "id", st.ID, "repository", st.RepositoryFullName, "profile", st.ProfileName, "status", st.Status)
		writeJSON(w, http.StatusAccepted, st)
		return
	}
	s.logger.Info("runner request reused", "id", st.ID, "repository", st.RepositoryFullName, "profile", st.ProfileName, "status", st.Status)
	writeJSON(w, http.StatusOK, st)
}

var (
	errRunnerConcurrencyLimit  = errors.New("runner concurrency limit reached")
	errProfileConcurrencyLimit = errors.New("profile concurrency limit reached")
)

func (s *Server) enqueueWorkflowJob(repositoryFullName, workflowRunName string, job github.WorkflowJob, payload []byte) (state.RunnerState, bool, error) {
	id := strconv.FormatInt(job.ID, 10)
	match, err := s.store.MatchProfile(repositoryFullName, job.Labels)
	if err != nil {
		return state.RunnerState{}, false, err
	}
	req := state.RunnerRequest{
		ID:                 id,
		Source:             "github_webhook",
		JobID:              job.ID,
		RepositoryFullName: repositoryFullName,
		RequestedLabels:    append([]string(nil), job.Labels...),
		Labels:             []string(job.Labels),
		RunnerName:         "e2b-" + id,
	}
	if match.Profile == nil {
		s.logger.Info("runner admission rejected", "id", req.ID, "repository", repositoryFullName, "labels", []string(job.Labels), "reason", match.Reason)
		st, err := s.rejectAdmission(req, payload, match.Reason)
		return st, false, err
	}
	req.ProfileName = match.Profile.Name
	req.RunnerGroup = match.Profile.RunnerGroup
	req.Labels = append([]string(nil), match.Profile.Labels...)
	s.logger.Info("workflow run job matched profile", "job_id", job.ID, "repository", repositoryFullName, "profile", match.Profile.Name, "runner_group", match.Profile.RunnerGroup, "labels", req.Labels)
	metrics.RecordWorkflowQueued(repositoryFullName, workflowRunName, job.Name, match.Profile.Name)
	return s.enqueueRunnerRequest(req, payload)
}

func (s *Server) enqueueRunnerRequest(req state.RunnerRequest, payload []byte) (state.RunnerState, bool, error) {
	s.admissionMu.Lock()
	if st, err := s.store.ReadState(req.ID); err == nil {
		s.admissionMu.Unlock()
		s.logger.Info("runner request already exists", "id", req.ID, "status", st.Status)
		return st, false, nil
	}
	active, err := s.store.ActiveCount()
	if err != nil {
		s.admissionMu.Unlock()
		return state.RunnerState{}, false, err
	}
	if active >= s.cfg.MaxConcurrentRunners {
		s.admissionMu.Unlock()
		s.logger.Info("runner global concurrency limit reached", "id", req.ID, "active", active, "limit", s.cfg.MaxConcurrentRunners)
		return state.RunnerState{}, false, errRunnerConcurrencyLimit
	}
	if rejected, err := s.rejectProfileConcurrency(req.ProfileName); err != nil {
		s.admissionMu.Unlock()
		return state.RunnerState{}, false, err
	} else if rejected {
		s.admissionMu.Unlock()
		s.logger.Info("runner profile concurrency limit reached", "id", req.ID, "profile", req.ProfileName)
		return state.RunnerState{}, false, errProfileConcurrencyLimit
	}
	created, st, err := s.store.CreateRequest(req, payload)
	s.admissionMu.Unlock()
	if err != nil {
		return state.RunnerState{}, false, err
	}
	if created {
		s.logger.Info("runner request created", "id", req.ID, "source", req.Source, "labels", req.Labels)
		s.store.AppendLog(req.ID, "control.log", []byte("runner request created\n"))
		s.refreshMetrics()
		s.signalQueue()
	}
	return st, created, nil
}

func (s *Server) rejectAdmission(req state.RunnerRequest, payload []byte, reason string) (state.RunnerState, error) {
	created, st, err := s.store.CreateRequest(req, payload)
	if err != nil {
		return state.RunnerState{}, err
	}
	if !created {
		s.logger.Info("runner admission rejection already exists", "id", req.ID, "status", st.Status, "reason", reason)
		return st, nil
	}
	st.Status = state.StatusFailed
	st.FailureStage = "admission"
	st.FailureReason = reason
	st.Error = "runner admission rejected"
	if err := s.store.WriteState(st); err != nil {
		return state.RunnerState{}, err
	}
	s.store.AppendLog(req.ID, "control.log", []byte("runner admission rejected: "+reason+"\n"))
	s.logger.Info("runner admission rejected and recorded", "id", req.ID, "repository", req.RepositoryFullName, "reason", reason)
	s.refreshMetrics()
	return st, nil
}

func (s *Server) startRunner(ctx context.Context, id, workerID string) {
	unlock := s.lockRunner(id)
	req, err := s.store.ReadRequest(id)
	if err != nil {
		unlock()
		s.logger.Error("read runner request", "id", id, "error", err)
		return
	}
	st, err := s.store.ReadState(id)
	if err != nil {
		unlock()
		s.logger.Error("read runner state", "id", id, "error", err)
		return
	}
	if st.Status == state.StatusCompleted || st.Status == state.StatusStopping {
		unlock()
		_ = s.store.ReleaseLease(id, workerID)
		s.logger.Info("runner start skipped because request is stopped", "id", id, "status", st.Status)
		s.store.AppendLog(id, "control.log", []byte("runner start skipped because request is stopped\n"))
		return
	}
	s.admissionMu.Lock()
	if req.ProfileName != "" {
		rejected, err := s.profileAtCapacity(req.ProfileName)
		if err != nil {
			s.admissionMu.Unlock()
			unlock()
			_ = s.store.ReleaseLease(id, workerID)
			s.logger.Error("check profile concurrency", "id", id, "profile", req.ProfileName, "error", err)
			return
		}
		if rejected {
			s.admissionMu.Unlock()
			unlock()
			_ = s.store.ReleaseLease(id, workerID)
			s.logger.Info("runner start deferred because profile is at capacity", "id", id, "profile", req.ProfileName)
			s.signalQueue()
			return
		}
	}
	st.Status = state.StatusCreating
	st.LeaseOwner = workerID
	st.LeaseExpiresAt = time.Now().UTC().Add(s.cfg.WorkerLeaseTTL)
	if err := s.store.WriteState(st); err != nil {
		s.admissionMu.Unlock()
		unlock()
		s.logger.Error("write creating state", "id", id, "error", err)
		return
	}
	s.admissionMu.Unlock()
	unlock()

	shouldStart, err := s.ensureWorkflowJobStillQueued(ctx, req)
	if err != nil {
		s.failStart(id, st, "github_job_status", err)
		return
	}
	if !shouldStart {
		_ = s.store.ReleaseLease(id, workerID)
		return
	}

	s.logger.Info("creating github registration token", "id", id)
	s.store.AppendLog(id, "control.log", []byte("creating github registration token\n"))
	createStartedAt := time.Now()
	token, err := s.gh.CreateRegistrationToken(ctx, req.RepositoryFullName)
	if err != nil {
		s.failStart(id, st, "github_registration", err)
		return
	}
	s.logger.Info("starting sandbox runner", "id", id, "runner_name", req.RunnerName)
	s.store.AppendLog(id, "control.log", []byte("starting sandbox runner\n"))
	profile, err := s.store.GetProfile(req.ProfileName)
	if err != nil {
		s.failStart(id, st, "profile_lookup", fmt.Errorf("load profile %q: %w", req.ProfileName, err))
		return
	}
	exitCh := make(chan struct{})
	createCtx, cancel := context.WithTimeout(ctx, s.cfg.SandboxCreateTimeout)
	repositoryURL, err := s.gh.RunnerURL(req.RepositoryFullName)
	if err != nil {
		cancel()
		s.failStart(id, st, "github_runner_url", err)
		return
	}
	result, err := s.sandbox.StartRunner(createCtx, sandboxrunner.StartInput{
		RequestID:         req.ID,
		RunnerName:        req.RunnerName,
		RepositoryURL:     repositoryURL,
		RegistrationToken: token.Token,
		Labels:            req.Labels,
		TemplateID:        profile.TemplateID,
		Timeout:           s.cfg.SandboxTimeout,
		CommandContext:    ctx,
		OnStdout:          func(data []byte) { s.appendRunnerStdout(id, data) },
		OnStderr:          func(data []byte) { s.store.AppendLog(id, "stderr.log", data) },
		OnExit: func(result sandboxrunner.ExitResult, err error) {
			defer close(exitCh)
			s.runnerExited(id, result, err)
		},
	})
	cancel()
	if err != nil {
		s.failStart(id, st, "sandbox_start", err)
		return
	}

	unlock = s.lockRunner(id)
	current, err := s.store.ReadState(id)
	if err != nil {
		unlock()
		s.logger.Error("read state before running update", "id", id, "error", err)
		s.cleanupStartedSandbox(id, result)
		return
	}
	if current.Status == state.StatusFailed || current.Status == state.StatusCompleted || current.Status == state.StatusStopping {
		unlock()
		s.logger.Info("runner exited before running update", "id", id, "status", current.Status)
		s.cleanupStartedSandbox(id, result)
		return
	}
	st = current
	st.Status = state.StatusRunning
	st.SandboxID = result.SandboxID
	st.ProcessPID = result.PID
	st.Error = ""
	st.LeaseOwner = ""
	st.LeaseExpiresAt = time.Time{}
	if err := s.store.WriteState(st); err != nil {
		unlock()
		s.logger.Error("write running state", "id", id, "error", err)
		s.store.AppendLog(id, "control.log", []byte("write running state failed: "+err.Error()+"\n"))
		s.cleanupStartedSandbox(id, result)
		return
	}
	unlock()
	s.logger.Info("sandbox runner started", "id", id, "sandbox_id", result.SandboxID, "pid", result.PID)
	s.store.AppendLog(id, "control.log", []byte(fmt.Sprintf("sandbox runner started sandbox_id=%s pid=%d\n", result.SandboxID, result.PID)))
	metrics.RecordCreate(req.ProfileName, time.Since(createStartedAt), "success")
	metrics.RecordWorkflowStarted(req.RepositoryFullName, "", req.RunnerName, req.ProfileName)
	s.refreshMetrics()
	<-exitCh
}

func (s *Server) ensureWorkflowJobStillQueued(ctx context.Context, req state.RunnerRequest) (bool, error) {
	if req.JobID == 0 {
		return true, nil
	}
	s.logger.Info("checking workflow job status before sandbox start", "id", req.ID, "job_id", req.JobID, "repository", req.RepositoryFullName)
	s.store.AppendLog(req.ID, "control.log", []byte("checking workflow job status before sandbox start\n"))
	job, err := s.gh.GetWorkflowJob(ctx, req.RepositoryFullName, req.JobID)
	if err != nil {
		return false, err
	}
	if job.Status == "queued" {
		s.logger.Info("workflow job is still queued", "id", req.ID, "job_id", req.JobID, "repository", req.RepositoryFullName)
		return true, nil
	}
	reason := strings.TrimSpace(job.Status)
	if job.Conclusion != "" {
		reason += "/" + job.Conclusion
	}
	if reason == "" {
		reason = "not_queued"
	}
	s.completeWithoutSandbox(req.ID, job, "workflow job is "+reason)
	return false, nil
}

func (s *Server) completeWithoutSandbox(id string, job github.WorkflowJob, reason string) {
	unlock := s.lockRunner(id)
	defer unlock()

	st, err := s.store.ReadState(id)
	if err != nil {
		s.logger.Error("read runner state for skip", "id", id, "error", err)
		return
	}
	if st.Status == state.StatusCompleted || st.Status == state.StatusStopping || st.Status == state.StatusRunning {
		s.logger.Info("runner skip ignored because state already advanced", "id", id, "status", st.Status, "reason", reason)
		return
	}
	if shouldRecordAssignedJob(st, job) {
		st.AssignedJobID = job.ID
		st.AssignedJobName = job.Name
	}
	st.Status = state.StatusCompleted
	st.CompletedAt = time.Now().UTC()
	st.Error = ""
	st.FailureStage = ""
	st.FailureReason = ""
	st.LeaseOwner = ""
	st.LeaseExpiresAt = time.Time{}
	if err := s.store.WriteState(st); err != nil {
		s.logger.Error("write skipped runner state", "id", id, "error", err)
		return
	}
	s.logger.Info("runner skipped before sandbox start", "id", id, "job_id", job.ID, "job_status", job.Status, "job_conclusion", job.Conclusion, "reason", reason)
	s.store.AppendLog(id, "control.log", []byte("runner skipped before sandbox start: "+reason+"\n"))
	s.refreshMetrics()
}

func (s *Server) appendRunnerStdout(id string, data []byte) {
	s.store.AppendLog(id, "stdout.log", data)
	text := string(data)
	if strings.Contains(text, "Listening for Jobs") {
		s.logger.Info("runner is listening for jobs", "id", id)
	}
	if strings.Contains(text, "Running job:") {
		s.markRunnerJobStarted(id)
	}
}

func (s *Server) markRunnerJobStarted(id string) {
	unlock := s.lockRunner(id)
	defer unlock()

	st, err := s.store.ReadState(id)
	if err != nil {
		s.logger.Error("read runner state for job started marker", "id", id, "error", err)
		return
	}
	if st.AssignedJobID != 0 || st.AssignedJobName == runnerJobStartedMarker || st.Status == state.StatusCompleted || st.Status == state.StatusStopping {
		return
	}
	st.AssignedJobName = runnerJobStartedMarker
	if err := s.store.WriteState(st); err != nil {
		s.logger.Error("write runner job started marker", "id", id, "error", err)
		return
	}
	s.logger.Info("runner accepted a job", "id", id)
	s.store.AppendLog(id, "control.log", []byte("runner accepted a job\n"))
}

func (s *Server) failStart(id string, st state.RunnerState, stage string, err error) {
	unlock := s.lockRunner(id)
	defer unlock()

	current, readErr := s.store.ReadState(id)
	if readErr == nil {
		if current.Status == state.StatusCompleted || current.Status == state.StatusStopping {
			_ = s.store.ReleaseLease(id, s.workerID)
			s.logger.Info("runner failure ignored because request is stopped", "id", id, "status", current.Status, "error", err)
			return
		}
		st = current
	}
	result := s.applyFailure(&st, stage, err, true)
	s.logger.Error("runner failed", "id", id, "error", err)
	s.store.AppendLog(id, "control.log", []byte(result.logLine+"\n"))
	if writeErr := s.store.WriteState(st); writeErr != nil {
		s.logger.Error("write failed state", "id", id, "error", writeErr)
	}
	metrics.RecordCreate(st.ProfileName, 0, result.metricResult)
	if st.Status == state.StatusQueued {
		s.signalQueue()
	}
	s.refreshMetrics()
}

func (s *Server) runnerExited(id string, result sandboxrunner.ExitResult, err error) {
	unlock := s.lockRunner(id)
	defer unlock()

	st, readErr := s.store.ReadState(id)
	if readErr != nil {
		s.logger.Error("read state after runner exit", "id", id, "error", readErr)
		return
	}
	if st.Status == state.StatusCompleted || st.Status == state.StatusStopping {
		return
	}
	if err != nil {
		st.Status = state.StatusFailed
		st.Error = err.Error()
		s.logger.Error("runner process exited with error", "id", id, "error", err)
		s.store.AppendLog(id, "control.log", []byte("runner process exited with error: "+err.Error()+"\n"))
		if cleanupErr := s.cleanupSandboxAfterExit(id, st); cleanupErr != nil {
			st.Error = st.Error + "; cleanup sandbox: " + cleanupErr.Error()
		}
		s.writeStateOrLog(id, st, "write failed exit state")
		return
	}
	if result.ExitCode == 0 {
		s.logger.Info("runner process exited", "id", id, "exit_code", result.ExitCode)
		s.store.AppendLog(id, "control.log", []byte("runner process exited cleanly\n"))
	} else {
		st.Status = state.StatusFailed
		st.Error = runnerExitMessage(result)
		s.logger.Error("runner process exited non-zero", "id", id, "exit_code", result.ExitCode, "stderr", result.Stderr, "runner_error", result.Error)
		s.store.AppendLog(id, "control.log", []byte(st.Error+"\n"))
	}
	if cleanupErr := s.cleanupSandboxAfterExit(id, st); cleanupErr != nil {
		st.Status = state.StatusFailed
		st.Error = "cleanup sandbox after runner exit: " + cleanupErr.Error()
	} else if st.Status != state.StatusFailed {
		st.Status = state.StatusCompleted
		st.CompletedAt = time.Now().UTC()
	}
	s.writeStateOrLog(id, st, "write exited state")
	s.refreshMetrics()
}

func (s *Server) cleanupSandboxAfterExit(id string, st state.RunnerState) error {
	if st.SandboxID == "" {
		return nil
	}
	if err := s.stopSandboxWithTimeout(context.Background(), st.SandboxID, st.ProcessPID); err != nil {
		if isSandboxGone(err) {
			s.logger.Info("sandbox already gone after runner exit", "id", id, "sandbox_id", st.SandboxID, "error", err)
			s.store.AppendLog(id, "control.log", []byte("sandbox already gone after runner exit: "+err.Error()+"\n"))
			return nil
		}
		s.logger.Error("cleanup sandbox after runner exit", "id", id, "sandbox_id", st.SandboxID, "error", err)
		s.store.AppendLog(id, "control.log", []byte("cleanup sandbox after runner exit failed: "+err.Error()+"\n"))
		return err
	}
	s.logger.Info("sandbox cleaned after runner exit", "id", id, "sandbox_id", st.SandboxID)
	s.store.AppendLog(id, "control.log", []byte("sandbox cleaned after runner exit\n"))
	return nil
}

func (s *Server) recoverRunner(ctx context.Context, id string) error {
	unlock := s.lockRunner(id)
	defer unlock()

	st, err := s.store.ReadState(id)
	if err != nil {
		return err
	}
	if !isActiveStatus(st.Status) {
		return nil
	}
	s.logger.Info("recovering runner after restart", "id", id, "status", st.Status, "sandbox_id", st.SandboxID)
	s.store.AppendLog(id, "control.log", []byte("recovering runner after service restart\n"))
	if st.SandboxID != "" {
		if err := s.stopSandboxWithTimeout(ctx, st.SandboxID, st.ProcessPID); err != nil {
			if isSandboxGone(err) {
				s.logger.Info("sandbox already gone during recovery", "id", id, "sandbox_id", st.SandboxID, "error", err)
				s.store.AppendLog(id, "control.log", []byte("sandbox already gone during recovery: "+err.Error()+"\n"))
			} else {
				st.Status = state.StatusFailed
				st.FailureStage = "recovery"
				st.FailureReason = "cleanup_failed"
				st.Error = "recover cleanup sandbox: " + err.Error()
				s.recordAudit("recovery", "runner.recovery_failed", "runner_request", st.ID, map[string]any{"error": st.Error})
				return s.store.WriteState(st)
			}
		}
	}
	st.Status = state.StatusFailed
	st.FailureStage = "recovery"
	st.FailureReason = "interrupted_runner"
	st.Error = "runner interrupted by runnerd restart"
	st.LeaseOwner = ""
	st.LeaseExpiresAt = time.Time{}
	st.CompletedAt = time.Time{}
	if err := s.store.WriteState(st); err != nil {
		return err
	}
	s.store.AppendLog(id, "control.log", []byte("runner marked failed during recovery\n"))
	s.recordAudit("recovery", "runner.recovered", "runner_request", st.ID, map[string]any{
		"status":         st.Status,
		"failure_reason": st.FailureReason,
	})
	s.refreshMetrics()
	return nil
}

func (s *Server) stopIfExists(ctx context.Context, id string, job github.WorkflowJob) {
	if _, err := s.store.ReadState(id); err != nil {
		s.logger.Info("stop skipped because runner state does not exist", "id", id)
		return
	}
	if _, err := s.stopRunner(ctx, id, job); err != nil {
		s.logger.Error("stop runner", "id", id, "error", err)
	}
}

func (s *Server) stopRunner(ctx context.Context, id string, job github.WorkflowJob) (state.RunnerState, error) {
	unlock := s.lockRunner(id)
	defer unlock()

	st, err := s.store.ReadState(id)
	if err != nil {
		s.logger.Error("read runner state for stop", "id", id, "error", err)
		return state.RunnerState{}, err
	}
	s.logger.Info("runner stop requested", "id", id, "status", st.Status, "sandbox_id", st.SandboxID, "pid", st.ProcessPID, "job_id", job.ID)
	if st.Status == state.StatusCompleted {
		if shouldRecordAssignedJob(st, job) {
			st.AssignedJobID = job.ID
			st.AssignedJobName = job.Name
			if err := s.store.WriteState(st); err != nil {
				return state.RunnerState{}, fmt.Errorf("write completed job assignment: %w", err)
			}
			s.logger.Info("recorded completed runner job assignment", "id", id, "job_id", job.ID, "job_name", job.Name)
		}
		s.logger.Info("runner stop skipped because already completed", "id", id)
		return st, nil
	}
	if shouldRecordAssignedJob(st, job) {
		st.AssignedJobID = job.ID
		st.AssignedJobName = job.Name
	}
	st.Status = state.StatusStopping
	stopStartedAt := time.Now()
	if err := s.store.WriteState(st); err != nil {
		return state.RunnerState{}, fmt.Errorf("write stopping state: %w", err)
	}
	s.logger.Info("runner marked stopping", "id", id, "sandbox_id", st.SandboxID, "pid", st.ProcessPID)
	st.Version++
	if st.SandboxID != "" {
		if err := s.stopSandboxWithTimeout(ctx, st.SandboxID, st.ProcessPID); err != nil {
			if isSandboxGone(err) {
				s.logger.Info("sandbox already gone", "id", id, "sandbox_id", st.SandboxID, "error", err)
				s.store.AppendLog(id, "control.log", []byte("sandbox already gone: "+err.Error()+"\n"))
			} else {
				if s.scheduleStopRetry(&st, err) {
					if writeErr := s.store.WriteState(st); writeErr != nil {
						return state.RunnerState{}, fmt.Errorf("schedule stop retry: %v; write stopping state: %w", err, writeErr)
					}
					s.store.AppendLog(id, "control.log", []byte(fmt.Sprintf("stop retry scheduled for %s: %s\n", st.NextRetryAt.Format(time.RFC3339), err)))
					s.logger.Info("runner stop retry scheduled", "id", id, "sandbox_id", st.SandboxID, "next_retry_at", st.NextRetryAt, "error", err)
					s.refreshMetrics()
					return st, nil
				}
				st.Status = state.StatusFailed
				st.FailureStage = "stop"
				st.FailureReason = "stop_failed"
				st.Error = err.Error()
				if writeErr := s.store.WriteState(st); writeErr != nil {
					return state.RunnerState{}, fmt.Errorf("stop sandbox: %v; write failed state: %w", err, writeErr)
				}
				s.logger.Error("runner stop failed", "id", id, "sandbox_id", st.SandboxID, "error", err)
				metrics.RecordStop(st.ProfileName, time.Since(stopStartedAt), "failed")
				s.refreshMetrics()
				return st, err
			}
		}
	}
	st.Status = state.StatusCompleted
	st.CompletedAt = time.Now().UTC()
	st.Error = ""
	if err := s.store.WriteState(st); err != nil {
		return state.RunnerState{}, err
	}
	s.logger.Info("runner stopped", "id", id, "sandbox_id", st.SandboxID, "duration_ms", time.Since(stopStartedAt).Milliseconds())
	metrics.RecordStop(st.ProfileName, time.Since(stopStartedAt), "success")
	s.refreshMetrics()
	return st, nil
}

func (s *Server) writeStateOrLog(id string, st state.RunnerState, msg string) {
	if err := s.store.WriteState(st); err != nil {
		s.logger.Error(msg, "id", id, "error", err)
		s.store.AppendLog(id, "control.log", []byte(msg+": "+err.Error()+"\n"))
	}
}

func runnerExitMessage(result sandboxrunner.ExitResult) string {
	detail := strings.TrimSpace(result.Stderr)
	if detail == "" {
		detail = strings.TrimSpace(result.Error)
	}
	if detail == "" {
		detail = strings.TrimSpace(result.Stdout)
	}
	if detail == "" {
		return fmt.Sprintf("runner process exited with code %d", result.ExitCode)
	}
	return fmt.Sprintf("runner process exited with code %d: %s", result.ExitCode, detail)
}

type failureResult struct {
	metricResult string
	logLine      string
}

func (s *Server) applyFailure(st *state.RunnerState, stage string, err error, allowRetry bool) failureResult {
	now := time.Now().UTC()
	code, retryable := classifyRetryableError(stage, err)
	st.Error = err.Error()
	st.FailureStage = stage
	st.FailureReason = code
	st.LastErrorCode = code
	st.LastErrorMessage = err.Error()
	st.LastErrorRetryable = retryable
	st.LeaseOwner = ""
	st.LeaseExpiresAt = time.Time{}
	st.SandboxID = ""
	st.ProcessPID = 0
	st.CompletedAt = time.Time{}
	if allowRetry && retryable && st.RetryCount < s.cfg.RetryMaxAttempts {
		st.Status = state.StatusQueued
		st.RetryCount++
		st.NextRetryAt = s.nextRetryAt(st.RetryCount, now)
		st.CreatingAt = time.Time{}
		st.RunningAt = time.Time{}
		st.StoppingAt = time.Time{}
		st.FailedAt = time.Time{}
		return failureResult{
			metricResult: "retry",
			logLine:      fmt.Sprintf("runner failed at %s, retry %d/%d scheduled for %s: %s", stage, st.RetryCount, s.cfg.RetryMaxAttempts, st.NextRetryAt.Format(time.RFC3339), err),
		}
	}
	st.Status = state.StatusFailed
	st.NextRetryAt = time.Time{}
	return failureResult{
		metricResult: "failed",
		logLine:      fmt.Sprintf("runner failed at %s: %s", stage, err),
	}
}

func (s *Server) nextRetryAt(retryCount int, now time.Time) time.Time {
	delay := s.cfg.RetryBaseDelay
	for i := 1; i < retryCount; i++ {
		delay *= 2
		if delay >= s.cfg.RetryMaxDelay {
			delay = s.cfg.RetryMaxDelay
			break
		}
	}
	if delay > s.cfg.RetryMaxDelay {
		delay = s.cfg.RetryMaxDelay
	}
	return now.Add(delay)
}

func classifyRetryableError(stage string, err error) (string, bool) {
	if err == nil {
		return "", false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout", true
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "failed to place sandbox"):
		return "sandbox_capacity", true
	case strings.Contains(msg, "secondary rate limit"):
		return "github_secondary_rate_limit", true
	case strings.Contains(msg, "status 408"), strings.Contains(msg, "status 409"), strings.Contains(msg, "status 425"), strings.Contains(msg, "status 429"):
		return "http_retryable_status", true
	case strings.Contains(msg, "status 5"):
		if strings.Contains(stage, "github") {
			return "github_server_error", true
		}
		return "backend_server_error", true
	case strings.Contains(msg, "timeout"), strings.Contains(msg, "timed out"), strings.Contains(msg, "i/o timeout"), strings.Contains(msg, "connection reset"), strings.Contains(msg, "temporary"), strings.Contains(msg, "eof"):
		return "temporary_network_error", true
	case strings.Contains(msg, "status 401"), strings.Contains(msg, "status 403"):
		return "auth_error", false
	default:
		return stage, false
	}
}

func shouldRecordAssignedJob(st state.RunnerState, job github.WorkflowJob) bool {
	if job.ID == 0 {
		return false
	}
	return st.AssignedJobID != job.ID || st.AssignedJobName != job.Name
}

func (s *Server) signalQueue() {
	select {
	case s.queueNotify <- struct{}{}:
	default:
	}
}

func (s *Server) retryOrFail(st state.RunnerState, stage string, err error) {
	unlock := s.lockRunner(st.ID)
	defer unlock()
	current, readErr := s.store.ReadState(st.ID)
	if readErr != nil {
		s.logger.Error("read state for retry/fail", "id", st.ID, "error", readErr)
		return
	}
	if current.Status == state.StatusCompleted || current.Status == state.StatusFailed {
		return
	}
	result := s.applyFailure(&current, stage, err, true)
	if writeErr := s.store.WriteState(current); writeErr != nil {
		s.logger.Error("write retry/fail state", "id", st.ID, "error", writeErr)
		return
	}
	s.store.AppendLog(st.ID, "control.log", []byte(result.logLine+"\n"))
	if current.Status == state.StatusQueued {
		s.signalQueue()
	}
	s.refreshMetrics()
}

func (s *Server) scheduleStopRetry(st *state.RunnerState, err error) bool {
	code, retryable := classifyRetryableError("stop", err)
	if !retryable || st.RetryCount >= s.cfg.RetryMaxAttempts {
		return false
	}
	st.RetryCount++
	st.Status = state.StatusStopping
	st.FailureStage = "stop"
	st.FailureReason = code
	st.LastErrorCode = code
	st.LastErrorMessage = err.Error()
	st.LastErrorRetryable = true
	st.Error = err.Error()
	st.NextRetryAt = s.nextRetryAt(st.RetryCount, time.Now().UTC())
	return true
}

func (s *Server) rejectProfileConcurrency(profileName string) (bool, error) {
	if strings.TrimSpace(profileName) == "" {
		return false, nil
	}
	return s.profileAtActiveLimit(profileName)
}

func (s *Server) profileAtCapacity(profileName string) (bool, error) {
	profile, err := s.store.GetProfile(profileName)
	if err != nil {
		return false, err
	}
	if profile.MaxConcurrency <= 0 {
		return false, nil
	}
	inFlight, err := s.store.InFlightCountForProfile(profileName)
	if err != nil {
		return false, err
	}
	return inFlight >= profile.MaxConcurrency, nil
}

func (s *Server) profileAtActiveLimit(profileName string) (bool, error) {
	profile, err := s.store.GetProfile(profileName)
	if err != nil {
		return false, err
	}
	if profile.MaxConcurrency <= 0 {
		return false, nil
	}
	active, err := s.store.ActiveCountForProfile(profileName)
	if err != nil {
		return false, err
	}
	return active >= profile.MaxConcurrency, nil
}

func (s *Server) ensureRepositoryAllowsProfile(repositoryFullName string, profile state.RunnerProfile, requestedLabels []string) error {
	policies, err := s.store.ListRepositoryPolicies()
	if err != nil {
		return err
	}
	allowed := profile.DefaultAvailable
	for _, policy := range policies {
		if !policy.Enabled || !repositoryPatternMatches(policy.RepositoryFullName, repositoryFullName) {
			continue
		}
		if policy.ProfileName == profile.Name {
			allowed = true
			break
		}
		if policy.RunnerGroupName != "" {
			group, err := s.store.GetRunnerGroup(policy.RunnerGroupName)
			if err != nil || !group.Enabled {
				continue
			}
			for _, specName := range group.SpecNames {
				if specName == profile.Name {
					allowed = true
					break
				}
			}
			if allowed {
				break
			}
		}
	}
	if !allowed {
		return fmt.Errorf("profile %q is not allowed for repository %q", profile.Name, repositoryFullName)
	}
	if len(requestedLabels) > 0 && !github.LabelsMatch(requestedLabels, profile.Labels) {
		return fmt.Errorf("requested labels do not satisfy profile %q", profile.Name)
	}
	return nil
}

func (s *Server) recordAudit(actor, action, resourceType, resourceID string, payload any) {
	var payloadJSON string
	if payload != nil {
		if data, err := json.Marshal(payload); err == nil {
			payloadJSON = string(data)
		}
	}
	event, err := s.store.AppendAuditEvent(state.AuditEvent{
		Actor:        actor,
		Action:       action,
		ResourceType: resourceType,
		ResourceID:   resourceID,
		PayloadJSON:  payloadJSON,
		CreatedAt:    time.Now().UTC(),
	})
	if err != nil {
		s.logger.Error("append audit event", "action", action, "resource_type", resourceType, "resource_id", resourceID, "error", err)
		return
	}
	s.logger.Info("audit event recorded", "id", event.ID, "actor", actor, "action", action, "resource_type", resourceType, "resource_id", resourceID)
}

func repositoryPatternMatches(pattern, repository string) bool {
	pattern = strings.TrimSpace(pattern)
	repository = strings.TrimSpace(repository)
	if pattern == "" || repository == "" {
		return false
	}
	if pattern == repository {
		return true
	}
	if strings.HasSuffix(pattern, "/*") {
		return strings.HasPrefix(repository, strings.TrimSuffix(pattern, "*"))
	}
	return false
}

func isActiveStatus(status string) bool {
	switch status {
	case state.StatusQueued, state.StatusCreating, state.StatusRunning, state.StatusStopping:
		return true
	default:
		return false
	}
}

type pprofAddress struct {
	Path    string
	Address string
}

func discoverPprofArtifacts() ([]pprofAddress, []string) {
	executable, err := os.Executable()
	if err != nil {
		return nil, nil
	}
	dir := filepath.Dir(executable)
	name := filepath.Base(executable)
	pprofFiles, _ := filepath.Glob(filepath.Join(dir, name+"_*_*.pprof"))
	scriptFiles, _ := filepath.Glob(filepath.Join(dir, name+"_*_profile_dump.sh"))
	addresses := make([]pprofAddress, 0, len(pprofFiles))
	for _, file := range pprofFiles {
		data, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		addresses = append(addresses, pprofAddress{
			Path:    file,
			Address: strings.TrimSpace(string(data)),
		})
	}
	return addresses, scriptFiles
}

func (s *Server) lockRunner(id string) func() {
	h := fnv.New32a()
	_, _ = h.Write([]byte(id))
	mu := &s.locks[int(h.Sum32())%len(s.locks)]
	mu.Lock()
	return func() {
		mu.Unlock()
	}
}

func (s *Server) cleanupStartedSandbox(id string, result sandboxrunner.StartResult) {
	if result.SandboxID == "" {
		s.logger.Info("cleanup started sandbox skipped because sandbox id is empty", "id", id)
		return
	}
	s.logger.Info("cleanup started sandbox", "id", id, "sandbox_id", result.SandboxID, "pid", result.PID)
	if err := s.stopSandboxWithTimeout(context.Background(), result.SandboxID, result.PID); err != nil && !isSandboxGone(err) {
		s.logger.Error("cleanup started sandbox", "id", id, "sandbox_id", result.SandboxID, "error", err)
		s.store.AppendLog(id, "control.log", []byte("cleanup started sandbox failed: "+err.Error()+"\n"))
		return
	}
	s.logger.Info("cleaned started sandbox", "id", id, "sandbox_id", result.SandboxID)
	s.store.AppendLog(id, "control.log", []byte("cleaned started sandbox\n"))
}

func (s *Server) stopSandboxWithTimeout(ctx context.Context, sandboxID string, pid uint32) error {
	if ctx == nil {
		ctx = context.Background()
	}
	s.logger.Info("stopping sandbox", "sandbox_id", sandboxID, "pid", pid, "timeout", s.cfg.SandboxStopTimeout.String())
	stopCtx, cancel := context.WithTimeout(ctx, s.cfg.SandboxStopTimeout)
	defer cancel()
	if err := s.sandbox.StopRunner(stopCtx, sandboxID, pid); err != nil {
		s.logger.Error("stop sandbox failed", "sandbox_id", sandboxID, "pid", pid, "error", err)
		return err
	}
	s.logger.Info("sandbox stopped", "sandbox_id", sandboxID, "pid", pid)
	return nil
}

func (s *Server) requireAdminAuth(w http.ResponseWriter, r *http.Request) bool {
	auth := r.Header.Get("Authorization")
	token := ""
	if strings.HasPrefix(auth, "Bearer ") {
		token = strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	}
	if token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(s.cfg.AdminToken)) == 1 {
		return true
	}
	s.logger.Warn("admin auth rejected", "method", r.Method, "path", r.URL.Path, "remote_addr", r.RemoteAddr, "has_authorization", auth != "")
	w.Header().Set("WWW-Authenticate", `Bearer realm="runnerd"`)
	writeError(w, http.StatusUnauthorized, "missing or invalid admin token")
	return false
}

func isSandboxGone(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "status 404") ||
		strings.Contains(msg, "Sandbox can't be resumed") ||
		strings.Contains(msg, "SandboxNotFound")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return strings.ToLower(hex.EncodeToString(b[:]))
}
