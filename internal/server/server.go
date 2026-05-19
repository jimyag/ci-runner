package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
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
	pendingMu   sync.Mutex
	pending     map[string]bool
	queueNotify chan struct{}
	startOnce   sync.Once
}

type manualCreateRequest struct {
	ID                 string   `json:"id"`
	RepositoryFullName string   `json:"repository_full_name"`
	ProfileName        string   `json:"profile_name"`
	Labels             []string `json:"labels"`
}

type upsertProfileRequest struct {
	Name           string   `json:"name"`
	Labels         []string `json:"labels"`
	TemplateID     string   `json:"template_id"`
	RunnerGroup    string   `json:"runner_group"`
	MaxConcurrency int      `json:"max_concurrency"`
	MinIdle        int      `json:"min_idle"`
	Priority       int      `json:"priority"`
	Enabled        *bool    `json:"enabled"`
}

type upsertRepositoryPolicyRequest struct {
	RepositoryFullName string `json:"repository_full_name"`
	ProfileName        string `json:"profile_name"`
	Enabled            *bool  `json:"enabled"`
}

type profileMatchRequest struct {
	RepositoryFullName string   `json:"repository_full_name"`
	Labels             []string `json:"labels"`
}

//go:embed admin/*
var adminAssets embed.FS

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
		pending:     map[string]bool{},
		queueNotify: make(chan struct{}, 1),
	}
	s.routes()
	s.startBackgroundLoops()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) Recover(ctx context.Context) error {
	states, err := s.store.ListStates()
	if err != nil {
		return err
	}
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
	s.mux.HandleFunc("POST /runners", s.handleCreateRunner)
	s.mux.HandleFunc("GET /runners", s.handleListRunners)
	s.mux.HandleFunc("GET /runners/{id}", s.handleGetRunner)
	s.mux.HandleFunc("GET /runners/{id}/logs/{name}", s.handleGetRunnerLog)
	s.mux.HandleFunc("DELETE /runners/{id}", s.handleDeleteRunner)
	s.mux.HandleFunc("GET /profiles", s.handleListProfiles)
	s.mux.HandleFunc("POST /profiles", s.handleCreateProfile)
	s.mux.HandleFunc("POST /profiles/match-test", s.handleMatchProfile)
	s.mux.HandleFunc("GET /profiles/{name}", s.handleGetProfile)
	s.mux.HandleFunc("PATCH /profiles/{name}", s.handlePatchProfile)
	s.mux.HandleFunc("DELETE /profiles/{name}", s.handleDeleteProfile)
	s.mux.HandleFunc("GET /repository-policies", s.handleListRepositoryPolicies)
	s.mux.HandleFunc("POST /repository-policies", s.handleCreateRepositoryPolicy)
	s.mux.HandleFunc("PATCH /repository-policies/{id}", s.handlePatchRepositoryPolicy)
	s.mux.HandleFunc("DELETE /repository-policies/{id}", s.handleDeleteRepositoryPolicy)
	s.mux.HandleFunc("GET /diagnostics/pprof", s.handleDiagnosticsPprof)
	s.mux.HandleFunc("GET /diagnostics/vars", s.handleDiagnosticsVars)
}

func (s *Server) startBackgroundLoops() {
	s.startOnce.Do(func() {
		go s.workerLoop()
		go s.sweeperLoop()
		go s.reconcilerLoop()
	})
}

func (s *Server) workerLoop() {
	for {
		select {
		case <-s.queueNotify:
		case <-time.After(500 * time.Millisecond):
		}
		s.processQueuedRequests()
	}
}

func (s *Server) sweeperLoop() {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		s.sweepOnce()
	}
}

func (s *Server) reconcilerLoop() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		s.reconcileOnce()
	}
}

func (s *Server) processQueuedRequests() {
	states, err := s.store.ListStates()
	if err != nil {
		s.logger.Error("list states for worker", "error", err)
		return
	}
	for _, st := range states {
		if st.Status != state.StatusQueued {
			continue
		}
		if s.markPending(st.ID) {
			go s.startRunnerWhenSlotAvailable(context.Background(), st.ID)
		}
	}
}

func (s *Server) refreshMetrics() {
	profiles, err := s.store.ListProfiles()
	if err != nil {
		return
	}
	states, err := s.store.ListStates()
	if err != nil {
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
				s.markFailed(st, "sweeper", "create_timeout", "runner create timed out")
			}
		case state.StatusRunning:
			if !st.RunningAt.IsZero() && now.Sub(st.RunningAt) > s.cfg.SandboxTimeout {
				s.stopIfExists(context.Background(), st.ID, github.WorkflowJob{})
			}
		case state.StatusStopping:
			if !st.StoppingAt.IsZero() && now.Sub(st.StoppingAt) > s.cfg.SandboxStopTimeout {
				s.stopIfExists(context.Background(), st.ID, github.WorkflowJob{})
			}
		}
	}
}

func (s *Server) reconcileOnce() {
	s.processQueuedRequests()
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
			if !st.StoppingAt.IsZero() && time.Since(st.StoppingAt) > s.cfg.SandboxStopTimeout {
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
		http.NotFound(w, r)
		return
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
		writeError(w, http.StatusBadRequest, "read body")
		return
	}
	if !github.VerifyWebhookSignature(s.cfg.GitHubWebhookSecret, body, r.Header.Get("X-Hub-Signature-256")) {
		writeError(w, http.StatusUnauthorized, "invalid signature")
		return
	}
	if r.Header.Get("X-GitHub-Event") != "workflow_job" {
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "ignored"})
		return
	}
	var event github.WorkflowJobEvent
	if err := json.Unmarshal(body, &event); err != nil {
		writeError(w, http.StatusBadRequest, "invalid workflow_job payload")
		return
	}
	id := strconv.FormatInt(event.WorkflowJob.ID, 10)
	switch event.Action {
	case "queued":
		match, err := s.store.MatchProfile(event.Repository.FullName, event.WorkflowJob.Labels)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		req := state.RunnerRequest{
			ID:                 id,
			Source:             "github_webhook",
			JobID:              event.WorkflowJob.ID,
			RepositoryFullName: event.Repository.FullName,
			Labels:             []string(event.WorkflowJob.Labels),
			RunnerName:         "e2b-" + id,
		}
		if match.Profile == nil {
			st, err := s.rejectAdmission(req, body, match.Reason)
			if err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSON(w, http.StatusAccepted, st)
			return
		}
		req.ProfileName = match.Profile.Name
		req.RunnerGroup = match.Profile.RunnerGroup
		req.Labels = append([]string(nil), match.Profile.Labels...)
		metrics.RecordWorkflowQueued(event.Repository.FullName, event.WorkflowRun.Name, event.WorkflowJob.Name, match.Profile.Name)
		s.createAndStart(w, r, req, body)
	case "completed":
		if !strings.HasPrefix(event.WorkflowJob.RunnerName, "e2b-") {
			s.logger.Info("completed workflow job has no managed runner", "job_id", id, "runner_name", event.WorkflowJob.RunnerName)
			writeJSON(w, http.StatusAccepted, map[string]string{"status": "ignored"})
			return
		}
		stopID := strings.TrimPrefix(event.WorkflowJob.RunnerName, "e2b-")
		st, err := s.stopRunner(context.Background(), stopID, event.WorkflowJob)
		if err != nil {
			s.logger.Error("stop runner", "id", stopID, "error", err)
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		metrics.RecordWorkflowCompleted(st.RepositoryFullName, event.WorkflowRun.Name, event.WorkflowJob.Name, st.ProfileName, "completed")
		writeJSON(w, http.StatusAccepted, st)
	default:
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "ignored"})
	}
}

func (s *Server) handleCreateRunner(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminAuth(w, r) {
		return
	}
	var input manualCreateRequest
	if r.Body != nil {
		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&input)
	}
	id := input.ID
	if id == "" {
		id = newID()
	}
	labels := input.Labels
	repositoryFullName := strings.TrimSpace(input.RepositoryFullName)
	if repositoryFullName == "" {
		repositoryFullName = s.cfg.DefaultRepositoryPattern()
	}
	profileName := strings.TrimSpace(input.ProfileName)
	runnerGroup := ""
	if profileName == "" {
		if len(labels) == 0 {
			labels = s.cfg.RunnerLabels
		}
		match, err := s.store.MatchProfile(repositoryFullName, labels)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if match.Profile == nil {
			writeError(w, http.StatusBadRequest, "no matching profile")
			return
		}
		profileName = match.Profile.Name
		runnerGroup = match.Profile.RunnerGroup
		labels = append([]string(nil), match.Profile.Labels...)
	} else {
		profile, err := s.store.GetProfile(profileName)
		if err != nil {
			writeError(w, http.StatusBadRequest, "profile not found")
			return
		}
		runnerGroup = profile.RunnerGroup
		if len(labels) == 0 {
			labels = append([]string(nil), profile.Labels...)
		}
	}
	req := state.RunnerRequest{
		ID:                 id,
		Source:             "manual_api",
		RepositoryFullName: repositoryFullName,
		Labels:             labels,
		ProfileName:        profileName,
		RunnerGroup:        runnerGroup,
		RunnerName:         "e2b-" + id,
	}
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
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, st)
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
		Name:           input.Name,
		Labels:         input.Labels,
		TemplateID:     input.TemplateID,
		RunnerGroup:    input.RunnerGroup,
		MaxConcurrency: input.MaxConcurrency,
		MinIdle:        input.MinIdle,
		Priority:       input.Priority,
		Enabled:        enabled,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
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
	profile, err := s.store.UpsertProfile(current)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.refreshMetrics()
	writeJSON(w, http.StatusOK, profile)
}

func (s *Server) handleDeleteProfile(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminAuth(w, r) {
		return
	}
	if err := s.store.DeleteProfile(r.PathValue("name")); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.refreshMetrics()
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
		Enabled:            enabled,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
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
	}
	if input.Enabled != nil {
		existing.Enabled = *input.Enabled
	}
	policy, err := s.store.UpsertRepositoryPolicy(*existing)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
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
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
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
		writeError(w, http.StatusInternalServerError, err.Error())
		return
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
			"backend":  "sqlite",
			"database": filepath.Join(s.cfg.StateDir, "runnerd.db"),
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
	resp, err := http.Get(strings.TrimRight(addresses[0].Address, "/") + "/debug/vars")
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
	s.admissionMu.Lock()
	if st, err := s.store.ReadState(req.ID); err == nil {
		s.admissionMu.Unlock()
		s.logger.Info("runner request already exists", "id", req.ID, "status", st.Status)
		writeJSON(w, http.StatusOK, st)
		return
	}
	active, err := s.store.ActiveCount()
	if err != nil {
		s.admissionMu.Unlock()
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if active >= s.cfg.MaxConcurrentRunners {
		s.admissionMu.Unlock()
		writeError(w, http.StatusTooManyRequests, "runner concurrency limit reached")
		return
	}
	created, st, err := s.store.CreateRequest(req, payload)
	s.admissionMu.Unlock()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if created {
		s.logger.Info("runner request created", "id", req.ID, "source", req.Source, "labels", req.Labels)
		s.store.AppendLog(req.ID, "control.log", []byte("runner request created\n"))
		s.refreshMetrics()
		s.signalQueue()
		writeJSON(w, http.StatusAccepted, st)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) rejectAdmission(req state.RunnerRequest, payload []byte, reason string) (state.RunnerState, error) {
	created, st, err := s.store.CreateRequest(req, payload)
	if err != nil {
		return state.RunnerState{}, err
	}
	if !created {
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
	s.refreshMetrics()
	return st, nil
}

func (s *Server) startRunnerWhenSlotAvailable(ctx context.Context, id string) {
	defer s.clearPending(id)
	s.store.AppendLog(id, "control.log", []byte("waiting for runner slot\n"))
	s.slots <- struct{}{}
	defer func() { <-s.slots }()
	s.store.AppendLog(id, "control.log", []byte("runner slot acquired\n"))
	s.startRunner(ctx, id)
}

func (s *Server) startRunner(ctx context.Context, id string) {
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
		s.logger.Info("runner start skipped because request is stopped", "id", id, "status", st.Status)
		s.store.AppendLog(id, "control.log", []byte("runner start skipped because request is stopped\n"))
		return
	}
	st.Status = state.StatusCreating
	if err := s.store.WriteState(st); err != nil {
		unlock()
		s.logger.Error("write creating state", "id", id, "error", err)
		return
	}
	unlock()

	s.logger.Info("creating github registration token", "id", id)
	s.store.AppendLog(id, "control.log", []byte("creating github registration token\n"))
	createStartedAt := time.Now()
	token, err := s.gh.CreateRegistrationToken(ctx)
	if err != nil {
		s.failState(id, st, err)
		return
	}
	s.logger.Info("starting sandbox runner", "id", id, "runner_name", req.RunnerName)
	s.store.AppendLog(id, "control.log", []byte("starting sandbox runner\n"))
	profile, err := s.store.GetProfile(req.ProfileName)
	if err != nil {
		s.failState(id, st, fmt.Errorf("load profile %q: %w", req.ProfileName, err))
		return
	}
	exitCh := make(chan struct{})
	createCtx, cancel := context.WithTimeout(ctx, s.cfg.SandboxCreateTimeout)
	result, err := s.sandbox.StartRunner(createCtx, sandboxrunner.StartInput{
		RequestID:         req.ID,
		RunnerName:        req.RunnerName,
		RepositoryURL:     s.gh.RunnerURL(),
		RegistrationToken: token.Token,
		Labels:            req.Labels,
		TemplateID:        profile.TemplateID,
		Timeout:           s.cfg.SandboxTimeout,
		OnStdout:          func(data []byte) { s.store.AppendLog(id, "stdout.log", data) },
		OnStderr:          func(data []byte) { s.store.AppendLog(id, "stderr.log", data) },
		OnExit: func(result sandboxrunner.ExitResult, err error) {
			defer close(exitCh)
			s.runnerExited(id, result, err)
		},
	})
	cancel()
	if err != nil {
		s.failState(id, st, err)
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

func (s *Server) failState(id string, st state.RunnerState, err error) {
	unlock := s.lockRunner(id)
	defer unlock()

	current, readErr := s.store.ReadState(id)
	if readErr == nil {
		if current.Status == state.StatusCompleted || current.Status == state.StatusStopping {
			s.logger.Info("runner failure ignored because request is stopped", "id", id, "status", current.Status, "error", err)
			return
		}
		st = current
	}
	st.Status = state.StatusFailed
	st.Error = err.Error()
	s.logger.Error("runner failed", "id", id, "error", err)
	s.store.AppendLog(id, "control.log", []byte("runner failed: "+err.Error()+"\n"))
	if writeErr := s.store.WriteState(st); writeErr != nil {
		s.logger.Error("write failed state", "id", id, "error", writeErr)
	}
	metrics.RecordCreate(st.ProfileName, 0, "failed")
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
		st.Error = fmt.Sprintf("runner process exited with code %d: %s", result.ExitCode, result.Stderr)
		s.logger.Error("runner process exited non-zero", "id", id, "exit_code", result.ExitCode, "stderr", result.Stderr)
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
				st.Error = "recover cleanup sandbox: " + err.Error()
				return s.store.WriteState(st)
			}
		}
	}
	st.Status = state.StatusCompleted
	st.CompletedAt = time.Now().UTC()
	st.Error = ""
	if err := s.store.WriteState(st); err != nil {
		return err
	}
	s.store.AppendLog(id, "control.log", []byte("runner marked completed during recovery\n"))
	s.refreshMetrics()
	return nil
}

func (s *Server) stopIfExists(ctx context.Context, id string, job github.WorkflowJob) {
	if _, err := s.store.ReadState(id); err != nil {
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
		return state.RunnerState{}, err
	}
	if st.Status == state.StatusCompleted {
		if shouldRecordAssignedJob(st, job) {
			st.AssignedJobID = job.ID
			st.AssignedJobName = job.Name
			if err := s.store.WriteState(st); err != nil {
				return state.RunnerState{}, fmt.Errorf("write completed job assignment: %w", err)
			}
		}
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
	st.Version++
	if st.SandboxID != "" {
		if err := s.stopSandboxWithTimeout(ctx, st.SandboxID, st.ProcessPID); err != nil {
			if isSandboxGone(err) {
				s.logger.Info("sandbox already gone", "id", id, "sandbox_id", st.SandboxID, "error", err)
				s.store.AppendLog(id, "control.log", []byte("sandbox already gone: "+err.Error()+"\n"))
			} else {
				st.Status = state.StatusFailed
				st.Error = err.Error()
				if writeErr := s.store.WriteState(st); writeErr != nil {
					return state.RunnerState{}, fmt.Errorf("stop sandbox: %v; write failed state: %w", err, writeErr)
				}
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

func (s *Server) markPending(id string) bool {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	if s.pending[id] {
		return false
	}
	s.pending[id] = true
	return true
}

func (s *Server) clearPending(id string) {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	delete(s.pending, id)
}

func (s *Server) markFailed(st state.RunnerState, stage, reason, message string) {
	unlock := s.lockRunner(st.ID)
	defer unlock()
	current, err := s.store.ReadState(st.ID)
	if err != nil {
		s.logger.Error("read state for failure mark", "id", st.ID, "error", err)
		return
	}
	if current.Status == state.StatusCompleted || current.Status == state.StatusFailed {
		return
	}
	current.Status = state.StatusFailed
	current.FailureStage = stage
	current.FailureReason = reason
	current.Error = message
	if err := s.store.WriteState(current); err != nil {
		s.logger.Error("write failed state", "id", st.ID, "error", err)
		return
	}
	s.store.AppendLog(st.ID, "control.log", []byte(message+"\n"))
	s.refreshMetrics()
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
		return
	}
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
	stopCtx, cancel := context.WithTimeout(ctx, s.cfg.SandboxStopTimeout)
	defer cancel()
	return s.sandbox.StopRunner(stopCtx, sandboxID, pid)
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
