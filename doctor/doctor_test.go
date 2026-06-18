package doctor_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hecatehq/acp-adapter-kit/doctor"
	adapterprocess "github.com/hecatehq/acp-adapter-kit/process"
)

func TestRunHappyPath(t *testing.T) {
	t.Setenv("GO_WANT_DOCTOR_HELPER", "1")
	t.Setenv("AGENT_API_KEY", "sk-agent-secret-doctor")
	t.Setenv("AGENT_HOME", "/tmp/codex-home")

	report, err := doctor.Run(context.Background(), doctor.Spec{
		AdapterName: "test adapter",
		Binary:      os.Args[0],
		VersionArgs: []string{"-test.run=TestDoctorHelper", "--", "version"},
		WorkDir:     t.TempDir(),
		InheritEnv:  []string{"GO_WANT_DOCTOR_HELPER"},
		EnvVars: []doctor.EnvVar{
			{Name: "AGENT_API_KEY"},
			{Name: "AGENT_HOME"},
		},
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if report.ResolvedCommand == "" {
		t.Fatal("ResolvedCommand is empty")
	}
	if !strings.Contains(report.VersionStdout, "fake-agent 1.2.3") {
		t.Fatalf("VersionStdout = %q, want fake version", report.VersionStdout)
	}
	if strings.Contains(report.VersionStdout, "sk-agent-secret-doctor") {
		t.Fatalf("VersionStdout leaked secret: %q", report.VersionStdout)
	}
	if !strings.Contains(report.VersionStdout, adapterprocess.RedactedValue) {
		t.Fatalf("VersionStdout = %q, want redacted value", report.VersionStdout)
	}
	if got := envPresent(report, "AGENT_API_KEY"); !got {
		t.Fatalf("AGENT_API_KEY present = %v, want true", got)
	}
	if got := envSensitive(report, "AGENT_API_KEY"); !got {
		t.Fatalf("AGENT_API_KEY sensitive = %v, want true", got)
	}
}

func TestRunMissingBinaryReturnsTypedError(t *testing.T) {
	_, err := doctor.Run(context.Background(), doctor.Spec{
		Binary:  filepath.Join(t.TempDir(), "missing-agent"),
		WorkDir: t.TempDir(),
	})
	var missing *adapterprocess.CommandNotFoundError
	if !errors.As(err, &missing) {
		t.Fatalf("error = %T %[1]v, want CommandNotFoundError", err)
	}
}

func TestRunRequiresBinary(t *testing.T) {
	_, err := doctor.Run(context.Background(), doctor.Spec{
		WorkDir: t.TempDir(),
	})
	if err == nil || !strings.Contains(err.Error(), "command is required") {
		t.Fatalf("error = %v, want command required", err)
	}
}

func TestRunRejectsShellBinary(t *testing.T) {
	_, err := doctor.Run(context.Background(), doctor.Spec{
		Binary:  "/bin/sh",
		WorkDir: t.TempDir(),
	})
	if err == nil || !strings.Contains(err.Error(), "is a shell") {
		t.Fatalf("error = %v, want shell rejection", err)
	}
}

func TestRunVersionProbeFailureIsSanitized(t *testing.T) {
	t.Setenv("GO_WANT_DOCTOR_HELPER", "1")
	t.Setenv("AGENT_API_KEY", "sk-agent-failing-secret")

	report, err := doctor.Run(context.Background(), doctor.Spec{
		Binary:      os.Args[0],
		VersionArgs: []string{"-test.run=TestDoctorHelper", "--", "fail"},
		WorkDir:     t.TempDir(),
		InheritEnv:  []string{"GO_WANT_DOCTOR_HELPER"},
		EnvVars:     []doctor.EnvVar{{Name: "AGENT_API_KEY"}},
	})
	var exitErr *adapterprocess.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("error = %T %[1]v, want ExitError", err)
	}
	if strings.Contains(report.VersionStderr, "sk-agent-failing-secret") {
		t.Fatalf("VersionStderr leaked secret: %q", report.VersionStderr)
	}
	if !strings.Contains(report.VersionStderr, adapterprocess.RedactedValue) {
		t.Fatalf("VersionStderr = %q, want redacted value", report.VersionStderr)
	}
}

func TestRunRejectsInvalidWorkingDirectory(t *testing.T) {
	_, err := doctor.Run(context.Background(), doctor.Spec{
		Binary:  os.Args[0],
		WorkDir: filepath.Join(t.TempDir(), "missing"),
	})
	if err == nil || !strings.Contains(err.Error(), "stat process working directory") {
		t.Fatalf("error = %v, want working-directory error", err)
	}
}

func TestWriteReportFormatsSuccess(t *testing.T) {
	var out bytes.Buffer
	doctor.WriteReport(&out, doctor.Report{
		AdapterName:     "test-adapter",
		Binary:          "agent",
		ResolvedCommand: "/usr/bin/agent",
		WorkDir:         "/tmp/project",
		VersionArgs:     []string{"--version"},
		VersionStdout:   "agent 1.2.3\n",
		Environment: []doctor.EnvStatus{{
			Name:      "AGENT_API_KEY",
			Present:   true,
			Required:  true,
			Sensitive: true,
		}},
	}, nil)

	want := strings.Join([]string{
		"test-adapter doctor: ok",
		"binary: agent",
		"resolved: /usr/bin/agent",
		"workdir: /tmp/project",
		"version args: --version",
		"env AGENT_API_KEY: present (redacted) (required)",
		"stdout:",
		"agent 1.2.3",
		"",
	}, "\n")
	if got := out.String(); got != want {
		t.Fatalf("report = %q, want %q", got, want)
	}
}

func TestWriteReportFormatsFailureAndTruncation(t *testing.T) {
	var out bytes.Buffer
	doctor.WriteReport(&out, doctor.Report{
		AdapterName:     "test-adapter",
		Binary:          "agent",
		VersionArgs:     []string{"--version"},
		VersionStderr:   "bad\n",
		StderrTruncated: true,
	}, errors.New("boom"))

	want := strings.Join([]string{
		"test-adapter doctor: failed",
		"binary: agent",
		"version args: --version",
		"stderr (truncated):",
		"bad",
		"",
	}, "\n")
	if got := out.String(); got != want {
		t.Fatalf("report = %q, want %q", got, want)
	}
}

func envPresent(report doctor.Report, name string) bool {
	for _, status := range report.Environment {
		if status.Name == name {
			return status.Present
		}
	}
	return false
}

func envSensitive(report doctor.Report, name string) bool {
	for _, status := range report.Environment {
		if status.Name == name {
			return status.Sensitive
		}
	}
	return false
}

func TestDoctorHelper(t *testing.T) {
	if os.Getenv("GO_WANT_DOCTOR_HELPER") != "1" {
		return
	}
	args := os.Args
	sep := -1
	for i, arg := range args {
		if arg == "--" {
			sep = i
			break
		}
	}
	if sep == -1 || sep+1 >= len(args) {
		os.Exit(2)
	}
	switch args[sep+1] {
	case "version":
		fmt.Printf("fake-agent 1.2.3 token=%s\n", os.Getenv("AGENT_API_KEY"))
	case "fail":
		fmt.Fprintf(os.Stderr, "bad token=%s\n", os.Getenv("AGENT_API_KEY"))
		os.Exit(9)
	default:
		os.Exit(2)
	}
	os.Exit(0)
}
