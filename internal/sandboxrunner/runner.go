package sandboxrunner

import (
	"context"
	_ "embed"
	"fmt"
	"net/http"
	"strings"
	"time"

	qnsandbox "github.com/qiniu/go-sdk/v7/sandbox"
)

type StartInput struct {
	RequestID         string
	RunnerName        string
	RepositoryURL     string
	RegistrationToken string
	Labels            []string
	TemplateID        string
	Timeout           time.Duration
	CommandContext    context.Context
	OnStdout          func([]byte)
	OnStderr          func([]byte)
	OnExit            func(ExitResult, error)
}

type StartResult struct {
	SandboxID string
	PID       uint32
}

type ExitResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
	Error    string
}

type Service interface {
	StartRunner(ctx context.Context, input StartInput) (StartResult, error)
	StopRunner(ctx context.Context, sandboxID string, pid uint32) error
}

type E2BService struct {
	client *qnsandbox.Client
}

//go:embed scripts/start-github-runner.sh
var startRunnerScriptTemplate string

func NewE2BService(apiKey, endpoint string, httpClient *http.Client) (*E2BService, error) {
	cfg := &qnsandbox.Config{APIKey: apiKey, Endpoint: endpoint, HTTPClient: httpClient}
	client, err := qnsandbox.NewClient(cfg)
	if err != nil {
		return nil, err
	}
	return &E2BService{client: client}, nil
}

func (s *E2BService) StartRunner(ctx context.Context, input StartInput) (StartResult, error) {
	timeout := int32(input.Timeout.Seconds())
	allowInternet := true
	metadata := qnsandbox.Metadata{
		"app":        "e2b-github-runner",
		"request_id": input.RequestID,
	}
	sb, _, err := s.client.CreateAndWait(ctx, qnsandbox.CreateParams{
		TemplateID:          input.TemplateID,
		Timeout:             &timeout,
		AllowInternetAccess: &allowInternet,
		Metadata:            &metadata,
	}, qnsandbox.WithPollInterval(500*time.Millisecond))
	if err != nil {
		return StartResult{}, err
	}

	if _, err := sb.Files().Write(ctx, "/tmp/start-github-runner.sh", []byte(startScript(input))); err != nil {
		_ = sb.Kill(ctx)
		return StartResult{}, fmt.Errorf("write runner script: %w", err)
	}
	commandCtx := input.CommandContext
	if commandCtx == nil {
		commandCtx = context.Background()
	}
	cmd := "chmod +x /tmp/start-github-runner.sh && /tmp/start-github-runner.sh"
	handle, err := sb.Commands().Start(
		commandCtx, cmd,
		qnsandbox.WithTag("github-runner"),
		qnsandbox.WithOnStdout(input.OnStdout),
		qnsandbox.WithOnStderr(input.OnStderr),
	)
	if err != nil {
		_ = sb.Kill(ctx)
		return StartResult{}, fmt.Errorf("start runner command: %w", err)
	}
	pid, err := handle.WaitPID(ctx)
	if err != nil {
		_ = sb.Kill(ctx)
		return StartResult{}, fmt.Errorf("wait runner pid: %w", err)
	}
	if input.OnExit != nil {
		go func() {
			result, err := handle.Wait()
			if result == nil {
				input.OnExit(ExitResult{}, err)
				return
			}
			input.OnExit(ExitResult{
				ExitCode: result.ExitCode,
				Stdout:   result.Stdout,
				Stderr:   result.Stderr,
				Error:    result.Error,
			}, err)
		}()
	}
	return StartResult{SandboxID: sb.ID(), PID: pid}, nil
}

func (s *E2BService) StopRunner(ctx context.Context, sandboxID string, pid uint32) error {
	sb, err := s.client.Connect(ctx, sandboxID, qnsandbox.ConnectParams{Timeout: 30})
	if err != nil {
		return err
	}
	if pid != 0 {
		_ = sb.Commands().Kill(ctx, pid)
	}
	return sb.Kill(ctx)
}

func startScript(input StartInput) string {
	labels := strings.Join(input.Labels, ",")
	return fmt.Sprintf(startRunnerScriptTemplate, input.RepositoryURL, input.RegistrationToken, input.RunnerName, labels)
}
