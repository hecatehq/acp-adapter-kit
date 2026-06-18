package runtimeproc

import (
	"context"
	"errors"
	"fmt"
	"io"

	adapterprocess "github.com/hecatehq/acp-adapter-kit/process"
)

const DefaultStderrLimit int64 = 64 * 1024

type Launcher interface {
	Launch(ctx context.Context, spec LaunchSpec) (*Process, error)
}

type ProcessLauncher struct {
	config Config
}

type Config struct {
	Binary      string
	Args        []string
	InheritEnv  []string
	StderrLimit int64
}

type LaunchSpec struct {
	Binary     string
	Args       []string
	WorkDir    string
	InheritEnv []string
	ExtraEnv   map[string]string
}

type Process struct {
	Command string
	Args    []string
	WorkDir string
	Stdin   io.WriteCloser
	Stdout  io.ReadCloser

	child *adapterprocess.Child
}

func DefaultConfig() Config {
	return Config{
		StderrLimit: DefaultStderrLimit,
	}
}

func NewLauncher(config Config) ProcessLauncher {
	if config.StderrLimit <= 0 {
		config.StderrLimit = DefaultStderrLimit
	}
	return ProcessLauncher{
		config: Config{
			Binary:      config.Binary,
			Args:        append([]string(nil), config.Args...),
			InheritEnv:  append([]string(nil), config.InheritEnv...),
			StderrLimit: config.StderrLimit,
		},
	}
}

func NewDefaultLauncher() ProcessLauncher {
	return NewLauncher(DefaultConfig())
}

func (l ProcessLauncher) Launch(ctx context.Context, spec LaunchSpec) (*Process, error) {
	if spec.WorkDir == "" {
		return nil, errors.New("runtime process workdir is required")
	}
	binary := firstNonEmpty(spec.Binary, l.config.Binary)
	args := append([]string(nil), l.config.Args...)
	args = append(args, spec.Args...)
	child, err := adapterprocess.Start(ctx, adapterprocess.StartSpec{
		Command: binary,
		Args:    args,
		Dir:     spec.WorkDir,
		Env: adapterprocess.EnvPolicy{
			Inherit: appendEnv(l.config.InheritEnv, spec.InheritEnv),
			Set:     cloneMap(spec.ExtraEnv),
		},
		StderrLimit: l.config.StderrLimit,
	})
	if err != nil {
		return nil, fmt.Errorf("launch runtime process: %w", err)
	}
	return &Process{
		Command: child.Command,
		Args:    append([]string(nil), child.Args...),
		WorkDir: child.Dir,
		Stdin:   child.Stdin,
		Stdout:  child.Stdout,
		child:   child,
	}, nil
}

func (p *Process) PID() int {
	if p == nil || p.child == nil {
		return 0
	}
	return p.child.PID()
}

func (p *Process) Kill() error {
	if p == nil || p.child == nil {
		return nil
	}
	return p.child.Kill()
}

func (p *Process) Wait() error {
	if p == nil || p.child == nil {
		return nil
	}
	return p.child.Wait()
}

func (p *Process) Stderr() []byte {
	if p == nil || p.child == nil {
		return nil
	}
	return p.child.Stderr()
}

func (p *Process) StderrTruncated() bool {
	if p == nil || p.child == nil {
		return false
	}
	return p.child.StderrTruncated()
}

func appendEnv(base []string, extra []string) []string {
	if len(base) == 0 && len(extra) == 0 {
		return nil
	}
	merged := make([]string, 0, len(base)+len(extra))
	merged = append(merged, base...)
	merged = append(merged, extra...)
	return merged
}

func cloneMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	clone := make(map[string]string, len(values))
	for key, value := range values {
		clone[key] = value
	}
	return clone
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
