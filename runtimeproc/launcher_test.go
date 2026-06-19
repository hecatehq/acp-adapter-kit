package runtimeproc_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	adapterprocess "github.com/hecatehq/acp-adapter-kit/process"
	"github.com/hecatehq/acp-adapter-kit/runtimeproc"
)

func TestDefaultConfigIsProviderNeutral(t *testing.T) {
	config := runtimeproc.DefaultConfig()
	if config.Binary != "" {
		t.Fatalf("Binary = %q, want empty provider-neutral default", config.Binary)
	}
	if len(config.InheritEnv) != 0 {
		t.Fatalf("InheritEnv = %#v, want empty provider-neutral default", config.InheritEnv)
	}
	if config.StderrLimit != runtimeproc.DefaultStderrLimit {
		t.Fatalf("StderrLimit = %d, want %d", config.StderrLimit, runtimeproc.DefaultStderrLimit)
	}
}

func TestLauncherLaunchesRuntimeProcess(t *testing.T) {
	t.Setenv("GO_WANT_RUNTIMEPROC_HELPER", "1")
	launcher := runtimeproc.NewLauncher(runtimeproc.Config{
		Binary:     os.Args[0],
		Args:       []string{"-test.run=TestRuntimeProcHelper", "--", "stream", "base-arg"},
		InheritEnv: []string{"GO_WANT_RUNTIMEPROC_HELPER"},
	})

	proc, err := launcher.Launch(context.Background(), runtimeproc.LaunchSpec{
		Args:    []string{"session-arg"},
		WorkDir: t.TempDir(),
		ExtraEnv: map[string]string{
			"VISIBLE": "yes",
		},
	})
	if err != nil {
		t.Fatalf("Launch returned error: %v", err)
	}
	if proc.PID() == 0 {
		t.Fatal("PID is 0")
	}
	if _, err := io.WriteString(proc.Stdin, "prompt text"); err != nil {
		t.Fatalf("write stdin: %v", err)
	}
	if err := proc.Stdin.Close(); err != nil {
		t.Fatalf("close stdin: %v", err)
	}
	stdout, err := io.ReadAll(proc.Stdout)
	if err != nil {
		t.Fatalf("read stdout: %v", err)
	}
	if err := proc.Wait(); err != nil {
		t.Fatalf("Wait returned error: %v; stderr=%s", err, proc.Stderr())
	}
	out := string(stdout)
	if !strings.Contains(out, "ARGS=base-arg,session-arg") {
		t.Fatalf("stdout = %q, want merged args", out)
	}
	if !strings.Contains(out, "VISIBLE=yes") {
		t.Fatalf("stdout = %q, want explicit env", out)
	}
	if !strings.Contains(out, "STDIN=prompt text") {
		t.Fatalf("stdout = %q, want stdin", out)
	}
}

func TestLauncherUsesSpecBinaryAndClonesConfigArgs(t *testing.T) {
	t.Setenv("GO_WANT_RUNTIMEPROC_HELPER", "1")
	configArgs := []string{"-test.run=TestRuntimeProcHelper", "--", "stream", "base-arg"}
	defaultBinary := filepath.Join(t.TempDir(), "unused-default-runtime")
	launcher := runtimeproc.NewLauncher(runtimeproc.Config{
		Binary:     defaultBinary,
		Args:       configArgs,
		InheritEnv: []string{"GO_WANT_RUNTIMEPROC_HELPER"},
	})
	configArgs[len(configArgs)-1] = "mutated-after-construction"

	proc, err := launcher.Launch(context.Background(), runtimeproc.LaunchSpec{
		Binary:  os.Args[0],
		Args:    []string{"session-arg"},
		WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Launch returned error: %v", err)
	}
	if _, err := io.WriteString(proc.Stdin, "prompt text"); err != nil {
		t.Fatalf("write stdin: %v", err)
	}
	if err := proc.Stdin.Close(); err != nil {
		t.Fatalf("close stdin: %v", err)
	}
	stdout, err := io.ReadAll(proc.Stdout)
	if err != nil {
		t.Fatalf("read stdout: %v", err)
	}
	if err := proc.Wait(); err != nil {
		t.Fatalf("Wait returned error: %v; stderr=%s", err, proc.Stderr())
	}

	out := string(stdout)
	if !strings.Contains(out, "ARGS=base-arg,session-arg") {
		t.Fatalf("stdout = %q, want cloned config args plus session args", out)
	}
	if strings.Contains(out, "mutated-after-construction") {
		t.Fatalf("stdout = %q, launcher retained caller-owned config args", out)
	}
	if proc.Command != filepath.Clean(os.Args[0]) {
		t.Fatalf("process command = %q, want LaunchSpec binary override %q", proc.Command, filepath.Clean(os.Args[0]))
	}
}

func TestLauncherRequiresWorkDir(t *testing.T) {
	launcher := runtimeproc.NewLauncher(runtimeproc.Config{Binary: os.Args[0]})
	_, err := launcher.Launch(context.Background(), runtimeproc.LaunchSpec{})
	if err == nil || !strings.Contains(err.Error(), "workdir is required") {
		t.Fatalf("Launch error = %v, want workdir required", err)
	}
}

func TestLauncherRequiresRuntimeBinary(t *testing.T) {
	launcher := runtimeproc.NewLauncher(runtimeproc.Config{})
	_, err := launcher.Launch(context.Background(), runtimeproc.LaunchSpec{WorkDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "command is required") {
		t.Fatalf("Launch error = %v, want command required", err)
	}
}

func TestLauncherRejectsShellBinary(t *testing.T) {
	launcher := runtimeproc.NewLauncher(runtimeproc.Config{Binary: "/bin/sh"})
	_, err := launcher.Launch(context.Background(), runtimeproc.LaunchSpec{WorkDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "is a shell") {
		t.Fatalf("Launch error = %v, want shell rejection", err)
	}
}

func TestLauncherReturnsExitErrorAndBoundedStderr(t *testing.T) {
	t.Setenv("GO_WANT_RUNTIMEPROC_HELPER", "1")
	launcher := runtimeproc.NewLauncher(runtimeproc.Config{
		Binary:      os.Args[0],
		Args:        []string{"-test.run=TestRuntimeProcHelper", "--", "fail"},
		InheritEnv:  []string{"GO_WANT_RUNTIMEPROC_HELPER"},
		StderrLimit: 10,
	})
	proc, err := launcher.Launch(context.Background(), runtimeproc.LaunchSpec{WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Launch returned error: %v", err)
	}
	var exitErr *adapterprocess.ExitError
	if err := proc.Wait(); !errors.As(err, &exitErr) {
		t.Fatalf("Wait error = %T %[1]v, want ExitError", err)
	}
	if exitErr.Code != 8 {
		t.Fatalf("exit code = %d, want 8", exitErr.Code)
	}
	if got := len(proc.Stderr()); got != 10 {
		t.Fatalf("stderr len = %d, want 10", got)
	}
	if !proc.StderrTruncated() {
		t.Fatal("StderrTruncated = false, want true")
	}
}

func TestLauncherCancelsRuntimeProcess(t *testing.T) {
	t.Setenv("GO_WANT_RUNTIMEPROC_HELPER", "1")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	launcher := runtimeproc.NewLauncher(runtimeproc.Config{
		Binary:     os.Args[0],
		Args:       []string{"-test.run=TestRuntimeProcHelper", "--", "sleep"},
		InheritEnv: []string{"GO_WANT_RUNTIMEPROC_HELPER"},
	})
	proc, err := launcher.Launch(ctx, runtimeproc.LaunchSpec{WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Launch returned error: %v", err)
	}
	waitErr := proc.Wait()
	if !errors.Is(waitErr, context.DeadlineExceeded) {
		t.Fatalf("Wait error = %v, want context deadline", waitErr)
	}
}

func TestRuntimeProcHelper(t *testing.T) {
	if os.Getenv("GO_WANT_RUNTIMEPROC_HELPER") != "1" {
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
	mode := args[sep+1]
	rest := args[sep+2:]
	switch mode {
	case "stream":
		stdin, _ := io.ReadAll(os.Stdin)
		fmt.Printf("ARGS=%s\n", strings.Join(rest, ","))
		fmt.Printf("VISIBLE=%s\n", os.Getenv("VISIBLE"))
		fmt.Printf("STDIN=%s\n", string(stdin))
	case "fail":
		fmt.Fprint(os.Stderr, strings.Repeat("e", 64))
		os.Exit(8)
	case "sleep":
		time.Sleep(5 * time.Second)
	default:
		os.Exit(2)
	}
	os.Exit(0)
}
