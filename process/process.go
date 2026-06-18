package process

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

const (
	DefaultOutputLimit = 1024 * 1024
	RedactedValue      = "[redacted]"
)

type Spec struct {
	Command     string
	Args        []string
	Dir         string
	Env         EnvPolicy
	StdoutLimit int64
	StderrLimit int64
}

type StartSpec struct {
	Command     string
	Args        []string
	Dir         string
	Env         EnvPolicy
	StderrLimit int64
}

type EnvPolicy struct {
	Inherit []string
	Set     map[string]string
}

type Result struct {
	Command         string
	Args            []string
	Dir             string
	Stdout          []byte
	Stderr          []byte
	StdoutTruncated bool
	StderrTruncated bool
}

type CommandNotFoundError struct {
	Command string
	Err     error
}

func (e *CommandNotFoundError) Error() string {
	return fmt.Sprintf("process command not found: %s", e.Command)
}

func (e *CommandNotFoundError) Unwrap() error {
	return e.Err
}

type ExitError struct {
	Command string
	Code    int
	Stderr  []byte
}

func (e *ExitError) Error() string {
	return fmt.Sprintf("process exited with code %d: %s", e.Code, e.Command)
}

type Child struct {
	Command string
	Args    []string
	Dir     string
	Stdin   io.WriteCloser
	Stdout  io.ReadCloser

	ctx      context.Context
	cmd      *exec.Cmd
	stderr   *limitedBuffer
	waitOnce sync.Once
	waitErr  error
}

func Run(ctx context.Context, spec Spec) (Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	resolved, err := ResolveCommand(spec.Command)
	if err != nil {
		return Result{}, err
	}
	if err := validateArgs(spec.Args); err != nil {
		return Result{}, err
	}
	dir, err := CleanWorkingDir(spec.Dir)
	if err != nil {
		return Result{}, err
	}
	env, err := BuildEnv(os.Environ(), spec.Env)
	if err != nil {
		return Result{}, err
	}

	stdout := newLimitedBuffer(limitOrDefault(spec.StdoutLimit))
	stderr := newLimitedBuffer(limitOrDefault(spec.StderrLimit))

	cmd := exec.CommandContext(ctx, resolved, spec.Args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	err = cmd.Run()
	result := Result{
		Command:         resolved,
		Args:            append([]string(nil), spec.Args...),
		Dir:             dir,
		Stdout:          stdout.Bytes(),
		Stderr:          stderr.Bytes(),
		StdoutTruncated: stdout.Truncated(),
		StderrTruncated: stderr.Truncated(),
	}
	if ctx.Err() != nil {
		return result, fmt.Errorf("process cancelled: %w", ctx.Err())
	}
	if err == nil {
		return result, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return result, &ExitError{Command: resolved, Code: exitErr.ExitCode(), Stderr: result.Stderr}
	}
	if os.IsNotExist(err) {
		return result, &CommandNotFoundError{Command: spec.Command, Err: err}
	}
	return result, fmt.Errorf("run process %q: %w", resolved, err)
}

func Start(ctx context.Context, spec StartSpec) (*Child, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	resolved, err := ResolveCommand(spec.Command)
	if err != nil {
		return nil, err
	}
	if err := validateArgs(spec.Args); err != nil {
		return nil, err
	}
	dir, err := CleanWorkingDir(spec.Dir)
	if err != nil {
		return nil, err
	}
	env, err := BuildEnv(os.Environ(), spec.Env)
	if err != nil {
		return nil, err
	}

	cmd := exec.CommandContext(ctx, resolved, spec.Args...)
	cmd.Dir = dir
	cmd.Env = env

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("open process stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("open process stdout: %w", err)
	}
	stderr := newLimitedBuffer(limitOrDefault(spec.StderrLimit))
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		if os.IsNotExist(err) {
			return nil, &CommandNotFoundError{Command: spec.Command, Err: err}
		}
		return nil, fmt.Errorf("start process %q: %w", resolved, err)
	}

	return &Child{
		Command: resolved,
		Args:    append([]string(nil), spec.Args...),
		Dir:     dir,
		Stdin:   stdin,
		Stdout:  stdout,
		ctx:     ctx,
		cmd:     cmd,
		stderr:  stderr,
	}, nil
}

func (c *Child) PID() int {
	if c == nil || c.cmd == nil || c.cmd.Process == nil {
		return 0
	}
	return c.cmd.Process.Pid
}

func (c *Child) Kill() error {
	if c == nil || c.cmd == nil || c.cmd.Process == nil {
		return nil
	}
	if err := c.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}

func (c *Child) Wait() error {
	if c == nil {
		return nil
	}
	c.waitOnce.Do(func() {
		err := c.cmd.Wait()
		if c.ctx.Err() != nil {
			c.waitErr = fmt.Errorf("process cancelled: %w", c.ctx.Err())
			return
		}
		if err == nil {
			return
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			c.waitErr = &ExitError{Command: c.Command, Code: exitErr.ExitCode(), Stderr: c.Stderr()}
			return
		}
		c.waitErr = fmt.Errorf("wait for process %q: %w", c.Command, err)
	})
	return c.waitErr
}

func (c *Child) Stderr() []byte {
	if c == nil || c.stderr == nil {
		return nil
	}
	return c.stderr.Bytes()
}

func (c *Child) StderrTruncated() bool {
	if c == nil || c.stderr == nil {
		return false
	}
	return c.stderr.Truncated()
}

func ResolveCommand(command string) (string, error) {
	if command == "" {
		return "", errors.New("process command is required")
	}
	if strings.ContainsRune(command, '\x00') {
		return "", errors.New("process command contains NUL byte")
	}
	if isShellCommand(command) {
		return "", fmt.Errorf("process command %q is a shell; use fixed argv without a shell", command)
	}
	if filepath.IsAbs(command) {
		return filepath.Clean(command), nil
	}
	if strings.ContainsRune(command, filepath.Separator) {
		return "", fmt.Errorf("process command path must be absolute: %s", command)
	}
	resolved, err := exec.LookPath(command)
	if err != nil {
		return "", &CommandNotFoundError{Command: command, Err: err}
	}
	if !filepath.IsAbs(resolved) {
		resolved, err = filepath.Abs(resolved)
		if err != nil {
			return "", fmt.Errorf("resolve process command %q: %w", command, err)
		}
	}
	return filepath.Clean(resolved), nil
}

func CleanWorkingDir(dir string) (string, error) {
	if dir == "" {
		return "", errors.New("process working directory is required")
	}
	if strings.ContainsRune(dir, '\x00') {
		return "", errors.New("process working directory contains NUL byte")
	}
	clean := filepath.Clean(dir)
	if !filepath.IsAbs(clean) {
		return "", fmt.Errorf("process working directory must be absolute: %s", dir)
	}
	info, err := os.Stat(clean)
	if err != nil {
		return "", fmt.Errorf("stat process working directory %q: %w", clean, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("process working directory is not a directory: %s", clean)
	}
	return clean, nil
}

func BuildEnv(base []string, policy EnvPolicy) ([]string, error) {
	allowed := make(map[string]struct{}, len(policy.Inherit))
	for _, name := range policy.Inherit {
		if err := validateEnvName(name); err != nil {
			return nil, err
		}
		allowed[name] = struct{}{}
	}

	values := map[string]string{}
	for _, entry := range base {
		name, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if _, keep := allowed[name]; keep {
			values[name] = value
		}
	}
	for name, value := range policy.Set {
		if err := validateEnvName(name); err != nil {
			return nil, err
		}
		if strings.ContainsRune(value, '\x00') {
			return nil, fmt.Errorf("process env %s contains NUL byte", name)
		}
		values[name] = value
	}

	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)

	env := make([]string, 0, len(names))
	for _, name := range names {
		env = append(env, name+"="+values[name])
	}
	return env, nil
}

func RedactEnv(env []string) []string {
	redacted := make([]string, 0, len(env))
	for _, entry := range env {
		name, _, ok := strings.Cut(entry, "=")
		if !ok {
			redacted = append(redacted, entry)
			continue
		}
		if IsSensitiveName(name) {
			redacted = append(redacted, name+"="+RedactedValue)
			continue
		}
		redacted = append(redacted, entry)
	}
	return redacted
}

func RedactArgs(args []string) []string {
	redacted := make([]string, len(args))
	redactNext := false
	for i, arg := range args {
		if redactNext {
			redacted[i] = RedactedValue
			redactNext = false
			continue
		}
		if strings.HasPrefix(arg, "-") {
			prefix := strings.TrimLeft(arg, "-")
			if name, _, ok := strings.Cut(prefix, "="); ok {
				if IsSensitiveName(name) {
					redacted[i] = arg[:strings.Index(arg, "=")+1] + RedactedValue
					continue
				}
			} else if IsSensitiveName(prefix) {
				redactNext = true
			}
		}
		redacted[i] = arg
	}
	return redacted
}

func IsSensitiveName(name string) bool {
	normalized := strings.ToUpper(strings.NewReplacer("-", "_", ".", "_").Replace(name))
	for _, token := range []string{"TOKEN", "SECRET", "PASSWORD", "PASS", "KEY", "CREDENTIAL", "AUTH", "COOKIE"} {
		if strings.Contains(normalized, token) {
			return true
		}
	}
	return false
}

func validateArgs(args []string) error {
	for i, arg := range args {
		if strings.ContainsRune(arg, '\x00') {
			return fmt.Errorf("process arg %d contains NUL byte", i)
		}
	}
	return nil
}

func validateEnvName(name string) error {
	if name == "" {
		return errors.New("process env name is required")
	}
	if strings.ContainsAny(name, "=\x00") {
		return fmt.Errorf("invalid process env name: %q", name)
	}
	return nil
}

func limitOrDefault(limit int64) int64 {
	if limit <= 0 {
		return DefaultOutputLimit
	}
	return limit
}

func isShellCommand(command string) bool {
	base := strings.ToLower(filepath.Base(command))
	base = strings.TrimSuffix(base, ".exe")
	switch base {
	case "sh", "bash", "zsh", "dash", "ksh", "fish", "cmd", "powershell", "pwsh":
		return true
	default:
		return false
	}
}

type limitedBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	limit     int64
	truncated bool
}

func newLimitedBuffer(limit int64) *limitedBuffer {
	return &limitedBuffer{limit: limit}
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	remaining := b.limit - int64(b.buf.Len())
	if remaining <= 0 {
		b.truncated = b.truncated || len(p) > 0
		return len(p), nil
	}
	if int64(len(p)) > remaining {
		_, _ = b.buf.Write(p[:remaining])
		b.truncated = true
		return len(p), nil
	}
	_, _ = b.buf.Write(p)
	return len(p), nil
}

func (b *limitedBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()

	return append([]byte(nil), b.buf.Bytes()...)
}

func (b *limitedBuffer) Truncated() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.truncated
}
