package sandboxrunner

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestStartScriptEncodesRunnerArguments(t *testing.T) {
	input := StartInput{
		RepositoryURL:     `https://github.com/o/r$(touch /tmp/pwn)`,
		RegistrationToken: `tok"$(touch /tmp/token)"`,
		RunnerName:        "runner`touch /tmp/name`",
		Labels:            []string{"self-hosted", `e2b$(touch /tmp/label)`},
		RunnerGroup:       `group$(touch /tmp/group)`,
	}
	script := startScript(input)

	for _, raw := range []string{
		input.RepositoryURL,
		input.RegistrationToken,
		input.RunnerName,
		strings.Join(input.Labels, ","),
		input.RunnerGroup,
	} {
		if strings.Contains(script, raw) {
			t.Fatalf("script contains raw argument %q:\n%s", raw, script)
		}
		if !strings.Contains(script, base64.StdEncoding.EncodeToString([]byte(raw))) {
			t.Fatalf("script does not contain encoded argument %q:\n%s", raw, script)
		}
	}
}

func TestStartScriptRunsCleanupAfterRunnerExit(t *testing.T) {
	script := startScript(StartInput{
		RepositoryURL:     "https://github.com/o/r",
		RegistrationToken: "token",
		RunnerName:        "runner",
		Labels:            []string{"self-hosted", "e2b"},
	})

	if strings.Contains(script, "exec ./run.sh") {
		t.Fatalf("script must not exec run.sh because that bypasses the EXIT cleanup trap:\n%s", script)
	}
	if !strings.Contains(script, "trap cleanup EXIT") || !strings.Contains(script, "./run.sh") {
		t.Fatalf("script does not preserve cleanup trap and runner execution:\n%s", script)
	}
}
