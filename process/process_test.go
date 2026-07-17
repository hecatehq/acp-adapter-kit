package process_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	adapterprocess "github.com/hecatehq/acp-adapter-kit/process"
)

func TestBuildEnvAllowlistAndOverrides(t *testing.T) {
	env, err := adapterprocess.BuildEnv([]string{
		"PATH=/bin",
		"OPENAI_API_KEY=secret",
		"HOME=/Users/test",
		"BROKEN",
	}, adapterprocess.EnvPolicy{
		Inherit: []string{"PATH", "HOME"},
		Set:     map[string]string{"HOME": "/sandbox/home", "CODEX_HOME": "/sandbox/codex"},
	})
	if err != nil {
		t.Fatalf("BuildEnv returned error: %v", err)
	}
	want := []string{"CODEX_HOME=/sandbox/codex", "HOME=/sandbox/home", "PATH=/bin"}
	if !reflect.DeepEqual(env, want) {
		t.Fatalf("env = %#v, want %#v", env, want)
	}
}

func TestRedactEnvAndArgs(t *testing.T) {
	env := adapterprocess.RedactEnv([]string{
		"OPENAI_API_KEY=sk-test",
		"PATH=/bin",
		"SESSION_TOKEN=tok",
	})
	wantEnv := []string{"OPENAI_API_KEY=[redacted]", "PATH=/bin", "SESSION_TOKEN=[redacted]"}
	if !reflect.DeepEqual(env, wantEnv) {
		t.Fatalf("redacted env = %#v, want %#v", env, wantEnv)
	}

	args := adapterprocess.RedactArgs([]string{
		"--api-key", "sk-test",
		"--token=tok",
		"--model", "gpt",
	})
	wantArgs := []string{"--api-key", "[redacted]", "--token=[redacted]", "--model", "gpt"}
	if !reflect.DeepEqual(args, wantArgs) {
		t.Fatalf("redacted args = %#v, want %#v", args, wantArgs)
	}
}

func TestRejectsShellCommands(t *testing.T) {
	_, err := adapterprocess.Run(context.Background(), adapterprocess.Spec{
		Command: "/bin/sh",
		Dir:     t.TempDir(),
	})
	if err == nil || !strings.Contains(err.Error(), "is a shell") {
		t.Fatalf("Run error = %v, want shell rejection", err)
	}
}

func TestRequiresAbsoluteWorkingDirectory(t *testing.T) {
	_, err := adapterprocess.CleanWorkingDir("relative")
	if err == nil || !strings.Contains(err.Error(), "must be absolute") {
		t.Fatalf("CleanWorkingDir error = %v, want absolute-dir error", err)
	}
}

func TestRunUsesFixedArgvAndAllowlistedEnv(t *testing.T) {
	command, args := helperCommand("argv-env", "literal;echo", "$HOME")
	result, err := adapterprocess.Run(context.Background(), adapterprocess.Spec{
		Command: command,
		Args:    args,
		Dir:     t.TempDir(),
		Env: adapterprocess.EnvPolicy{
			Inherit: []string{"PATH"},
			Set:     map[string]string{"GO_WANT_PROCESS_HELPER": "1", "VISIBLE": "yes"},
		},
		StdoutLimit: 1024,
		StderrLimit: 1024,
	})
	if err != nil {
		t.Fatalf("Run returned error: %v; stderr=%s", err, result.Stderr)
	}
	out := string(result.Stdout)
	if !strings.Contains(out, "ARGS=literal;echo,$HOME") {
		t.Fatalf("stdout = %q, want literal argv", out)
	}
	if !strings.Contains(out, "VISIBLE=yes") {
		t.Fatalf("stdout = %q, want explicit env", out)
	}
	if strings.Contains(out, "OPENAI_API_KEY=") {
		t.Fatalf("stdout = %q, leaked non-allowlisted env", out)
	}
}

func TestRunWithBaseEnvDoesNotConsultHostEnvironment(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "host-secret")
	command, args := helperCommand("argv-env", "base-env")
	result, err := adapterprocess.RunWithBaseEnv(context.Background(), adapterprocess.Spec{
		Command: command,
		Args:    args,
		Dir:     t.TempDir(),
		Env: adapterprocess.EnvPolicy{
			Inherit: []string{"VISIBLE", "OPENAI_API_KEY"},
			Set:     map[string]string{"GO_WANT_PROCESS_HELPER": "1"},
		},
		StdoutLimit: 1024,
		StderrLimit: 1024,
	}, []string{"VISIBLE=from-host-boundary"})
	if err != nil {
		t.Fatalf("RunWithBaseEnv returned error: %v; stderr=%s", err, result.Stderr)
	}
	out := string(result.Stdout)
	if !strings.Contains(out, "VISIBLE=from-host-boundary") {
		t.Fatalf("stdout = %q, want supplied base environment", out)
	}
	if strings.Contains(out, "OPENAI_API_KEY=") {
		t.Fatalf("stdout = %q, leaked process environment outside supplied base", out)
	}
}

func TestRunStreamWithBaseEnvUsesSuppliedEnvironment(t *testing.T) {
	command, args := helperCommand("argv-env", "stream-base-env")
	var streamed strings.Builder
	result, err := adapterprocess.RunStreamWithBaseEnv(context.Background(), adapterprocess.Spec{
		Command: command,
		Args:    args,
		Dir:     t.TempDir(),
		Env: adapterprocess.EnvPolicy{
			Inherit: []string{"VISIBLE"},
			Set:     map[string]string{"GO_WANT_PROCESS_HELPER": "1"},
		},
		StdoutLimit: 1024,
		StderrLimit: 1024,
	}, []string{"VISIBLE=stream-boundary"}, func(chunk []byte) error {
		streamed.Write(chunk)
		return nil
	})
	if err != nil {
		t.Fatalf("RunStreamWithBaseEnv returned error: %v; stderr=%s", err, result.Stderr)
	}
	if !strings.Contains(streamed.String(), "VISIBLE=stream-boundary") {
		t.Fatalf("streamed = %q, want supplied base environment", streamed.String())
	}
}

func TestOutputLimits(t *testing.T) {
	command, args := helperCommand("output")
	result, err := adapterprocess.Run(context.Background(), adapterprocess.Spec{
		Command:     command,
		Args:        args,
		Dir:         t.TempDir(),
		Env:         adapterprocess.EnvPolicy{Set: map[string]string{"GO_WANT_PROCESS_HELPER": "1"}},
		StdoutLimit: 10,
		StderrLimit: 12,
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if got := len(result.Stdout); got != 10 {
		t.Fatalf("stdout len = %d, want 10", got)
	}
	if got := len(result.Stderr); got != 12 {
		t.Fatalf("stderr len = %d, want 12", got)
	}
	if !result.StdoutTruncated || !result.StderrTruncated {
		t.Fatalf("truncated flags = stdout:%v stderr:%v, want both true", result.StdoutTruncated, result.StderrTruncated)
	}
}

func TestRunStreamEmitsStdoutAndCapturesResult(t *testing.T) {
	command, args := helperCommand("chunks")
	var streamed strings.Builder
	result, err := adapterprocess.RunStream(context.Background(), adapterprocess.Spec{
		Command:     command,
		Args:        args,
		Dir:         t.TempDir(),
		Env:         adapterprocess.EnvPolicy{Set: map[string]string{"GO_WANT_PROCESS_HELPER": "1"}},
		StdoutLimit: 1024,
		StderrLimit: 1024,
	}, func(chunk []byte) error {
		streamed.Write(chunk)
		return nil
	})
	if err != nil {
		t.Fatalf("RunStream returned error: %v; stderr=%s", err, result.Stderr)
	}
	if got, want := string(result.Stdout), "first chunk\nsecond chunk\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if streamed.String() != string(result.Stdout) {
		t.Fatalf("streamed = %q, want captured stdout %q", streamed.String(), result.Stdout)
	}
}

func TestRunStreamCallbackErrorFailsRun(t *testing.T) {
	command, args := helperCommand("chunks")
	result, err := adapterprocess.RunStream(context.Background(), adapterprocess.Spec{
		Command:     command,
		Args:        args,
		Dir:         t.TempDir(),
		Env:         adapterprocess.EnvPolicy{Set: map[string]string{"GO_WANT_PROCESS_HELPER": "1"}},
		StdoutLimit: 1024,
		StderrLimit: 1024,
	}, func([]byte) error {
		return errors.New("client closed")
	})
	if err == nil || !strings.Contains(err.Error(), "client closed") {
		t.Fatalf("RunStream error = %v, want callback error", err)
	}
	if len(result.Stdout) == 0 {
		t.Fatalf("stdout = %q, want captured bytes before callback failure", result.Stdout)
	}
}

func TestNonZeroExitReturnsExitError(t *testing.T) {
	command, args := helperCommand("exit")
	result, err := adapterprocess.Run(context.Background(), adapterprocess.Spec{
		Command:     command,
		Args:        args,
		Dir:         t.TempDir(),
		Env:         adapterprocess.EnvPolicy{Set: map[string]string{"GO_WANT_PROCESS_HELPER": "1"}},
		StdoutLimit: 1024,
		StderrLimit: 1024,
	})
	var exitErr *adapterprocess.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("error = %T %[1]v, want ExitError", err)
	}
	if exitErr.Code != 7 {
		t.Fatalf("exit code = %d, want 7", exitErr.Code)
	}
	if !strings.Contains(string(result.Stderr), "boom") {
		t.Fatalf("stderr = %q, want boom", result.Stderr)
	}
}

func TestContextCancellationKillsProcess(t *testing.T) {
	command, args := helperCommand("sleep")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := adapterprocess.Run(ctx, adapterprocess.Spec{
		Command:     command,
		Args:        args,
		Dir:         t.TempDir(),
		Env:         adapterprocess.EnvPolicy{Set: map[string]string{"GO_WANT_PROCESS_HELPER": "1"}},
		StdoutLimit: 1024,
		StderrLimit: 1024,
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context deadline", err)
	}
}

func TestStartUsesFixedArgvEnvAndPipes(t *testing.T) {
	command, args := helperCommand("stream", "literal;echo", "$HOME")
	child, err := adapterprocess.Start(context.Background(), adapterprocess.StartSpec{
		Command: command,
		Args:    args,
		Dir:     t.TempDir(),
		Env: adapterprocess.EnvPolicy{
			Inherit: []string{"PATH"},
			Set:     map[string]string{"GO_WANT_PROCESS_HELPER": "1", "VISIBLE": "yes"},
		},
		StderrLimit: 1024,
	})
	if err != nil {
		t.Fatalf("Start returned error: %v", err)
	}
	if child.PID() == 0 {
		t.Fatal("PID is 0")
	}
	if _, err := io.WriteString(child.Stdin, "stdin-data"); err != nil {
		t.Fatalf("write stdin: %v", err)
	}
	if err := child.Stdin.Close(); err != nil {
		t.Fatalf("close stdin: %v", err)
	}
	stdout, err := io.ReadAll(child.Stdout)
	if err != nil {
		t.Fatalf("read stdout: %v", err)
	}
	if err := child.Wait(); err != nil {
		t.Fatalf("Wait returned error: %v; stderr=%s", err, child.Stderr())
	}
	out := string(stdout)
	if !strings.Contains(out, "ARGS=literal;echo,$HOME") {
		t.Fatalf("stdout = %q, want literal argv", out)
	}
	if !strings.Contains(out, "VISIBLE=yes") {
		t.Fatalf("stdout = %q, want explicit env", out)
	}
	if !strings.Contains(out, "STDIN=stdin-data") {
		t.Fatalf("stdout = %q, want stdin data", out)
	}
	if strings.Contains(out, "OPENAI_API_KEY=") {
		t.Fatalf("stdout = %q, leaked non-allowlisted env", out)
	}
}

func TestStartCapturesStderrLimitAndExitError(t *testing.T) {
	command, args := helperCommand("start-exit")
	child, err := adapterprocess.Start(context.Background(), adapterprocess.StartSpec{
		Command:     command,
		Args:        args,
		Dir:         t.TempDir(),
		Env:         adapterprocess.EnvPolicy{Set: map[string]string{"GO_WANT_PROCESS_HELPER": "1"}},
		StderrLimit: 8,
	})
	if err != nil {
		t.Fatalf("Start returned error: %v", err)
	}
	var exitErr *adapterprocess.ExitError
	if err := child.Wait(); !errors.As(err, &exitErr) {
		t.Fatalf("Wait error = %T %[1]v, want ExitError", err)
	}
	if exitErr.Code != 6 {
		t.Fatalf("exit code = %d, want 6", exitErr.Code)
	}
	if got := len(child.Stderr()); got != 8 {
		t.Fatalf("stderr len = %d, want 8", got)
	}
	if !child.StderrTruncated() {
		t.Fatal("StderrTruncated = false, want true")
	}
}

func TestStartRejectsShellCommands(t *testing.T) {
	_, err := adapterprocess.Start(context.Background(), adapterprocess.StartSpec{
		Command: "/bin/sh",
		Dir:     t.TempDir(),
	})
	if err == nil || !strings.Contains(err.Error(), "is a shell") {
		t.Fatalf("Start error = %v, want shell rejection", err)
	}
}

func TestStartContextCancellationKillsProcess(t *testing.T) {
	command, args := helperCommand("sleep")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	child, err := adapterprocess.Start(ctx, adapterprocess.StartSpec{
		Command:     command,
		Args:        args,
		Dir:         t.TempDir(),
		Env:         adapterprocess.EnvPolicy{Set: map[string]string{"GO_WANT_PROCESS_HELPER": "1"}},
		StderrLimit: 1024,
	})
	if err != nil {
		t.Fatalf("Start returned error: %v", err)
	}
	waitErr := child.Wait()
	if !errors.Is(waitErr, context.DeadlineExceeded) {
		t.Fatalf("Wait error = %v, want context deadline", waitErr)
	}
}

func TestMissingBinaryReturnsTypedError(t *testing.T) {
	_, err := adapterprocess.Run(context.Background(), adapterprocess.Spec{
		Command: filepath.Join(t.TempDir(), "missing-binary"),
		Dir:     t.TempDir(),
	})
	var missing *adapterprocess.CommandNotFoundError
	if !errors.As(err, &missing) {
		t.Fatalf("error = %T %[1]v, want CommandNotFoundError", err)
	}
}

func helperCommand(mode string, args ...string) (string, []string) {
	command := os.Args[0]
	helperArgs := []string{"-test.run=TestProcessHelper", "--", mode}
	helperArgs = append(helperArgs, args...)
	return command, helperArgs
}

func TestProcessHelper(t *testing.T) {
	if os.Getenv("GO_WANT_PROCESS_HELPER") != "1" {
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
	case "argv-env":
		fmt.Printf("ARGS=%s\n", strings.Join(rest, ","))
		fmt.Printf("VISIBLE=%s\n", os.Getenv("VISIBLE"))
		if value := os.Getenv("OPENAI_API_KEY"); value != "" {
			fmt.Printf("OPENAI_API_KEY=%s\n", value)
		}
	case "output":
		fmt.Fprint(os.Stdout, strings.Repeat("o", 64))
		fmt.Fprint(os.Stderr, strings.Repeat("e", 64))
	case "chunks":
		fmt.Fprint(os.Stdout, "first chunk\n")
		time.Sleep(10 * time.Millisecond)
		fmt.Fprint(os.Stdout, "second chunk\n")
	case "exit":
		fmt.Fprint(os.Stderr, "boom")
		os.Exit(7)
	case "sleep":
		time.Sleep(5 * time.Second)
	case "stream":
		stdin, _ := io.ReadAll(os.Stdin)
		fmt.Printf("ARGS=%s\n", strings.Join(rest, ","))
		fmt.Printf("VISIBLE=%s\n", os.Getenv("VISIBLE"))
		fmt.Printf("STDIN=%s\n", string(stdin))
		if value := os.Getenv("OPENAI_API_KEY"); value != "" {
			fmt.Printf("OPENAI_API_KEY=%s\n", value)
		}
	case "start-exit":
		fmt.Fprint(os.Stderr, strings.Repeat("x", 64))
		os.Exit(6)
	default:
		os.Exit(2)
	}
	os.Exit(0)
}
