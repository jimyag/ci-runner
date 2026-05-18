package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jimyag/e2b-github-runner/internal/config"
	"github.com/jimyag/e2b-github-runner/internal/github"
	"github.com/jimyag/e2b-github-runner/internal/sandboxrunner"
	"github.com/jimyag/e2b-github-runner/internal/state"
)

type Server struct {
	cfg     config.Config
	store   *state.Store
	gh      *github.Client
	sandbox sandboxrunner.Service
	logger  *slog.Logger
	mux     *http.ServeMux
	slots   chan struct{}
}

type manualCreateRequest struct {
	ID     string   `json:"id"`
	Labels []string `json:"labels"`
}

func New(cfg config.Config, store *state.Store, gh *github.Client, sandbox sandboxrunner.Service, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{cfg: cfg, store: store, gh: gh, sandbox: sandbox, logger: logger, mux: http.NewServeMux(), slots: make(chan struct{}, cfg.MaxConcurrentRunners)}
	s.routes()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("POST /webhooks/github", s.handleGitHubWebhook)
	s.mux.HandleFunc("POST /runners", s.handleCreateRunner)
	s.mux.HandleFunc("GET /runners", s.handleListRunners)
	s.mux.HandleFunc("GET /runners/{id}", s.handleGetRunner)
	s.mux.HandleFunc("DELETE /runners/{id}", s.handleDeleteRunner)
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
		if !github.LabelsMatch(event.WorkflowJob.Labels, s.cfg.RunnerLabels) {
			writeJSON(w, http.StatusAccepted, map[string]string{"status": "ignored"})
			return
		}
		req := state.RunnerRequest{
			ID:         id,
			Source:     "github_webhook",
			JobID:      event.WorkflowJob.ID,
			Labels:     s.cfg.RunnerLabels,
			RunnerName: "e2b-" + id,
		}
		s.createAndStart(w, r, req, body)
	case "completed":
		stopID := id
		if strings.HasPrefix(event.WorkflowJob.RunnerName, "e2b-") {
			stopID = strings.TrimPrefix(event.WorkflowJob.RunnerName, "e2b-")
		}
		s.stopIfExists(r.Context(), stopID)
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "stopping"})
	default:
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "ignored"})
	}
}

func (s *Server) handleCreateRunner(w http.ResponseWriter, r *http.Request) {
	var input manualCreateRequest
	if r.Body != nil {
		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&input)
	}
	id := input.ID
	if id == "" {
		id = newID()
	}
	labels := input.Labels
	if len(labels) == 0 {
		labels = s.cfg.RunnerLabels
	}
	req := state.RunnerRequest{
		ID:         id,
		Source:     "manual_api",
		Labels:     labels,
		RunnerName: "e2b-" + id,
	}
	s.createAndStart(w, r, req, nil)
}

func (s *Server) handleListRunners(w http.ResponseWriter, r *http.Request) {
	states, err := s.store.ListStates()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, states)
}

func (s *Server) handleGetRunner(w http.ResponseWriter, r *http.Request) {
	st, err := s.store.ReadState(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "runner not found")
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleDeleteRunner(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	st, err := s.stopRunner(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, st)
}

func (s *Server) createAndStart(w http.ResponseWriter, r *http.Request, req state.RunnerRequest, payload []byte) {
	if st, err := s.store.ReadState(req.ID); err == nil {
		s.logger.Info("runner request already exists", "id", req.ID, "status", st.Status)
		writeJSON(w, http.StatusOK, st)
		return
	}
	active, err := s.store.ActiveCount()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if active >= s.cfg.MaxConcurrentRunners {
		writeError(w, http.StatusTooManyRequests, "runner concurrency limit reached")
		return
	}
	created, st, err := s.store.CreateRequest(req, payload)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if created {
		s.logger.Info("runner request created", "id", req.ID, "source", req.Source, "labels", req.Labels)
		s.store.AppendLog(req.ID, "control.log", []byte("runner request created\n"))
		go s.startRunnerWhenSlotAvailable(context.Background(), req.ID)
		writeJSON(w, http.StatusAccepted, st)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) startRunnerWhenSlotAvailable(ctx context.Context, id string) {
	s.store.AppendLog(id, "control.log", []byte("waiting for runner slot\n"))
	s.slots <- struct{}{}
	defer func() { <-s.slots }()
	s.store.AppendLog(id, "control.log", []byte("runner slot acquired\n"))
	s.startRunner(ctx, id)
}

func (s *Server) startRunner(ctx context.Context, id string) {
	req, err := s.store.ReadRequest(id)
	if err != nil {
		s.logger.Error("read runner request", "id", id, "error", err)
		return
	}
	st, err := s.store.ReadState(id)
	if err != nil {
		s.logger.Error("read runner state", "id", id, "error", err)
		return
	}
	st.Status = state.StatusCreating
	if err := s.store.WriteState(st); err != nil {
		s.logger.Error("write creating state", "id", id, "error", err)
		return
	}
	s.logger.Info("creating github registration token", "id", id)
	s.store.AppendLog(id, "control.log", []byte("creating github registration token\n"))
	token, err := s.gh.CreateRegistrationToken(ctx)
	if err != nil {
		s.failState(id, st, err)
		return
	}
	s.logger.Info("starting sandbox runner", "id", id, "runner_name", req.RunnerName)
	s.store.AppendLog(id, "control.log", []byte("starting sandbox runner\n"))
	exitCh := make(chan struct{})
	result, err := s.sandbox.StartRunner(ctx, sandboxrunner.StartInput{
		RequestID:         req.ID,
		RunnerName:        req.RunnerName,
		RepositoryURL:     s.gh.RunnerURL(),
		RegistrationToken: token.Token,
		Labels:            req.Labels,
		TemplateID:        s.cfg.SandboxTemplateID,
		Timeout:           s.cfg.SandboxTimeout,
		OnStdout:          func(data []byte) { s.store.AppendLog(id, "stdout.log", data) },
		OnStderr:          func(data []byte) { s.store.AppendLog(id, "stderr.log", data) },
		OnExit: func(result sandboxrunner.ExitResult, err error) {
			defer close(exitCh)
			s.runnerExited(id, result, err)
		},
	})
	if err != nil {
		s.failState(id, st, err)
		return
	}
	current, err := s.store.ReadState(id)
	if err != nil {
		s.logger.Error("read state before running update", "id", id, "error", err)
		return
	}
	if current.Status == state.StatusFailed || current.Status == state.StatusStopped || current.Status == state.StatusStopping {
		s.logger.Info("runner exited before running update", "id", id, "status", current.Status)
		return
	}
	st = current
	st.Status = state.StatusRunning
	st.SandboxID = result.SandboxID
	st.ProcessPID = result.PID
	st.Error = ""
	if err := s.store.WriteState(st); err != nil {
		s.logger.Error("write running state", "id", id, "error", err)
	}
	s.logger.Info("sandbox runner started", "id", id, "sandbox_id", result.SandboxID, "pid", result.PID)
	s.store.AppendLog(id, "control.log", []byte(fmt.Sprintf("sandbox runner started sandbox_id=%s pid=%d\n", result.SandboxID, result.PID)))
	<-exitCh
}

func (s *Server) failState(id string, st state.RunnerState, err error) {
	st.Status = state.StatusFailed
	st.Error = err.Error()
	s.logger.Error("runner failed", "id", id, "error", err)
	s.store.AppendLog(id, "control.log", []byte("runner failed: "+err.Error()+"\n"))
	if writeErr := s.store.WriteState(st); writeErr != nil {
		s.logger.Error("write failed state", "id", id, "error", writeErr)
	}
}

func (s *Server) runnerExited(id string, result sandboxrunner.ExitResult, err error) {
	st, readErr := s.store.ReadState(id)
	if readErr != nil {
		s.logger.Error("read state after runner exit", "id", id, "error", readErr)
		return
	}
	if st.Status == state.StatusStopped || st.Status == state.StatusStopping {
		return
	}
	if err != nil {
		st.Status = state.StatusFailed
		st.Error = err.Error()
		s.logger.Error("runner process exited with error", "id", id, "error", err)
		s.store.AppendLog(id, "control.log", []byte("runner process exited with error: "+err.Error()+"\n"))
		s.cleanupSandboxAfterExit(id, st)
		_ = s.store.WriteState(st)
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
	s.cleanupSandboxAfterExit(id, st)
	if st.Status != state.StatusFailed {
		st.Status = state.StatusStopped
		st.StoppedAt = time.Now().UTC()
	}
	_ = s.store.WriteState(st)
}

func (s *Server) cleanupSandboxAfterExit(id string, st state.RunnerState) {
	if st.SandboxID == "" {
		return
	}
	if err := s.sandbox.StopRunner(context.Background(), st.SandboxID, st.ProcessPID); err != nil {
		if isSandboxGone(err) {
			s.logger.Info("sandbox already gone after runner exit", "id", id, "sandbox_id", st.SandboxID, "error", err)
			s.store.AppendLog(id, "control.log", []byte("sandbox already gone after runner exit: "+err.Error()+"\n"))
			return
		}
		s.logger.Error("cleanup sandbox after runner exit", "id", id, "sandbox_id", st.SandboxID, "error", err)
		s.store.AppendLog(id, "control.log", []byte("cleanup sandbox after runner exit failed: "+err.Error()+"\n"))
		return
	}
	s.logger.Info("sandbox cleaned after runner exit", "id", id, "sandbox_id", st.SandboxID)
	s.store.AppendLog(id, "control.log", []byte("sandbox cleaned after runner exit\n"))
}

func (s *Server) stopIfExists(ctx context.Context, id string) {
	if _, err := s.store.ReadState(id); err != nil {
		return
	}
	if _, err := s.stopRunner(ctx, id); err != nil {
		s.logger.Error("stop runner", "id", id, "error", err)
	}
}

func (s *Server) stopRunner(ctx context.Context, id string) (state.RunnerState, error) {
	st, err := s.store.ReadState(id)
	if err != nil {
		return state.RunnerState{}, err
	}
	if st.Status == state.StatusStopped {
		return st, nil
	}
	st.Status = state.StatusStopping
	_ = s.store.WriteState(st)
	if st.SandboxID != "" {
		if err := s.sandbox.StopRunner(ctx, st.SandboxID, st.ProcessPID); err != nil {
			if isSandboxGone(err) {
				s.logger.Info("sandbox already gone", "id", id, "sandbox_id", st.SandboxID, "error", err)
				s.store.AppendLog(id, "control.log", []byte("sandbox already gone: "+err.Error()+"\n"))
			} else {
				st.Status = state.StatusFailed
				st.Error = err.Error()
				_ = s.store.WriteState(st)
				return st, err
			}
		}
	}
	st.Status = state.StatusStopped
	st.StoppedAt = time.Now().UTC()
	st.Error = ""
	if err := s.store.WriteState(st); err != nil {
		return state.RunnerState{}, err
	}
	return st, nil
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
