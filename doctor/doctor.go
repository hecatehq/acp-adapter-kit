package doctor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	adapterprocess "github.com/hecatehq/acp-adapter-kit/process"
)

const DefaultOutputLimit int64 = 64 * 1024

type Spec struct {
	AdapterName string
	Binary      string
	VersionArgs []string
	WorkDir     string
	InheritEnv  []string
	EnvVars     []EnvVar
	ExtraEnv    map[string]string
	StdoutLimit int64
	StderrLimit int64
}

type EnvVar struct {
	Name     string
	Required bool
}

type EnvStatus struct {
	Name      string `json:"name"`
	Present   bool   `json:"present"`
	Required  bool   `json:"required"`
	Sensitive bool   `json:"sensitive"`
}

type Report struct {
	AdapterName     string      `json:"adapter_name"`
	Binary          string      `json:"binary"`
	ResolvedCommand string      `json:"resolved_command,omitempty"`
	WorkDir         string      `json:"workdir,omitempty"`
	VersionArgs     []string    `json:"version_args"`
	VersionStdout   string      `json:"version_stdout,omitempty"`
	VersionStderr   string      `json:"version_stderr,omitempty"`
	StdoutTruncated bool        `json:"stdout_truncated,omitempty"`
	StderrTruncated bool        `json:"stderr_truncated,omitempty"`
	Environment     []EnvStatus `json:"environment,omitempty"`
}

func Run(ctx context.Context, spec Spec) (Report, error) {
	spec = withDefaults(spec)
	report := Report{
		AdapterName: spec.AdapterName,
		Binary:      spec.Binary,
		VersionArgs: append([]string(nil), spec.VersionArgs...),
	}

	resolved, err := adapterprocess.ResolveCommand(spec.Binary)
	if err != nil {
		return report, fmt.Errorf("find runtime binary: %w", err)
	}
	if err := validateResolvedCommand(spec.Binary, resolved); err != nil {
		return report, fmt.Errorf("find runtime binary: %w", err)
	}
	report.ResolvedCommand = resolved

	workDir, err := cleanWorkDir(spec.WorkDir)
	if err != nil {
		return report, err
	}
	report.WorkDir = workDir
	report.Environment = envStatuses(spec.EnvVars)

	result, err := adapterprocess.Run(ctx, adapterprocess.Spec{
		Command: resolved,
		Args:    spec.VersionArgs,
		Dir:     workDir,
		Env: adapterprocess.EnvPolicy{
			Inherit: inheritEnv(spec.InheritEnv, spec.EnvVars),
			Set:     cloneMap(spec.ExtraEnv),
		},
		StdoutLimit: spec.StdoutLimit,
		StderrLimit: spec.StderrLimit,
	})
	report.VersionStdout = redactOutput(string(result.Stdout), spec.EnvVars)
	report.VersionStderr = redactOutput(string(result.Stderr), spec.EnvVars)
	report.StdoutTruncated = result.StdoutTruncated
	report.StderrTruncated = result.StderrTruncated
	if err != nil {
		return report, fmt.Errorf("run version probe: %w", err)
	}
	return report, nil
}

func withDefaults(spec Spec) Spec {
	if len(spec.VersionArgs) == 0 {
		spec.VersionArgs = []string{"--version"}
	}
	if spec.StdoutLimit <= 0 {
		spec.StdoutLimit = DefaultOutputLimit
	}
	if spec.StderrLimit <= 0 {
		spec.StderrLimit = DefaultOutputLimit
	}
	return spec
}

func cleanWorkDir(dir string) (string, error) {
	if dir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("get working directory: %w", err)
		}
		dir = cwd
	}
	if !filepath.IsAbs(dir) {
		abs, err := filepath.Abs(dir)
		if err != nil {
			return "", fmt.Errorf("resolve working directory %q: %w", dir, err)
		}
		dir = abs
	}
	return adapterprocess.CleanWorkingDir(dir)
}

func validateResolvedCommand(command string, resolved string) error {
	info, err := os.Stat(resolved)
	if err != nil {
		if os.IsNotExist(err) {
			return &adapterprocess.CommandNotFoundError{Command: command, Err: err}
		}
		return fmt.Errorf("stat runtime binary %q: %w", resolved, err)
	}
	if info.IsDir() {
		return fmt.Errorf("runtime binary is a directory: %s", resolved)
	}
	return nil
}

func envStatuses(vars []EnvVar) []EnvStatus {
	statuses := make([]EnvStatus, 0, len(vars))
	for _, envVar := range vars {
		if envVar.Name == "" {
			continue
		}
		_, present := os.LookupEnv(envVar.Name)
		statuses = append(statuses, EnvStatus{
			Name:      envVar.Name,
			Present:   present,
			Required:  envVar.Required,
			Sensitive: adapterprocess.IsSensitiveName(envVar.Name),
		})
	}
	sort.Slice(statuses, func(i, j int) bool {
		return statuses[i].Name < statuses[j].Name
	})
	return statuses
}

func inheritEnv(base []string, vars []EnvVar) []string {
	seen := map[string]struct{}{}
	for _, name := range base {
		if name != "" {
			seen[name] = struct{}{}
		}
	}
	for _, envVar := range vars {
		if envVar.Name != "" {
			seen[envVar.Name] = struct{}{}
		}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
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

func redactOutput(output string, vars []EnvVar) string {
	redacted := output
	for _, envVar := range vars {
		if envVar.Name == "" || !adapterprocess.IsSensitiveName(envVar.Name) {
			continue
		}
		value := os.Getenv(envVar.Name)
		if len(value) < 4 {
			continue
		}
		redacted = strings.ReplaceAll(redacted, value, adapterprocess.RedactedValue)
	}
	return redacted
}
