package metrics

import (
	"expvar"
	"fmt"
	"time"

	"github.com/jimyag/e2b-github-runner/internal/state"
)

var (
	profileCurrent   = expvar.NewMap("e2b_runner_profile_current")
	profileDesired   = expvar.NewMap("e2b_runner_profile_desired")
	profilePending   = expvar.NewMap("e2b_runner_profile_pending")
	profileStatus    = expvar.NewMap("e2b_runner_profile_status")
	createDuration   = expvar.NewMap("e2b_runner_create_duration_ms_total")
	stopDuration     = expvar.NewMap("e2b_runner_stop_duration_ms_total")
	operationsTotal  = expvar.NewMap("e2b_runner_operations_total")
	workflowQueued   = expvar.NewMap("github_workflow_jobs_queued_total")
	workflowStarted  = expvar.NewMap("github_workflow_jobs_started_total")
	workflowComplete = expvar.NewMap("github_workflow_jobs_completed_total")
)

func Refresh(profiles []state.RunnerProfile, states []state.RunnerState) {
	current := map[string]int64{}
	pending := map[string]int64{}
	for _, st := range states {
		profile := st.ProfileName
		if profile == "" {
			profile = "unassigned"
		}
		switch st.Status {
		case state.StatusQueued, state.StatusCreating:
			pending[profile]++
		case state.StatusRunning, state.StatusStopping:
			current[profile]++
		}
	}
	profileCurrent.Init()
	profileDesired.Init()
	profilePending.Init()
	profileStatus.Init()
	for _, profile := range profiles {
		profileCurrent.Set(profile.Name, newInt(current[profile.Name]))
		profileDesired.Set(profile.Name, newInt(int64(profile.MaxConcurrency)))
		profilePending.Set(profile.Name, newInt(pending[profile.Name]))
		if profile.Enabled {
			profileStatus.Set(profile.Name, newInt(1))
		} else {
			profileStatus.Set(profile.Name, newInt(0))
		}
	}
}

func RecordCreate(profile string, d time.Duration, result string) {
	createDuration.Add(profile, d.Milliseconds())
	operationsTotal.Add(metricKey(profile, "create", result), 1)
}

func RecordStop(profile string, d time.Duration, result string) {
	stopDuration.Add(profile, d.Milliseconds())
	operationsTotal.Add(metricKey(profile, "stop", result), 1)
}

func RecordWorkflowQueued(repository, workflow, job, profile string) {
	workflowQueued.Add(metricKey(repository, workflow, job, profile), 1)
}

func RecordWorkflowStarted(repository, workflow, job, profile string) {
	workflowStarted.Add(metricKey(repository, workflow, job, profile), 1)
}

func RecordWorkflowCompleted(repository, workflow, job, profile, conclusion string) {
	workflowComplete.Add(metricKey(repository, workflow, job, profile, conclusion), 1)
}

func metricKey(parts ...string) string {
	return fmt.Sprintf("%q", join(parts...))
}

func join(parts ...string) string {
	out := ""
	for i, part := range parts {
		if i > 0 {
			out += "|"
		}
		out += part
	}
	return out
}

func newInt(v int64) *expvar.Int {
	value := new(expvar.Int)
	value.Set(v)
	return value
}
