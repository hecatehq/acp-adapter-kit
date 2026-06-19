package adaptercli

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/hecatehq/acp-adapter-kit/acp"
	"github.com/hecatehq/acp-adapter-kit/doctor"
	"github.com/hecatehq/acp-adapter-kit/runtimeacp"
	"github.com/hecatehq/acp-adapter-kit/runtimebridge"
	"github.com/hecatehq/acp-adapter-kit/runtimehost"
	"github.com/hecatehq/acp-adapter-kit/runtimeproc"
	"github.com/spf13/cobra"
)

type Spec struct {
	Info    acp.AdapterInfo
	Short   string
	Stdin   io.Reader
	Stdout  io.Writer
	Stderr  io.Writer
	Runtime RuntimeSpec
	Doctor  *DoctorSpec
}

type RuntimeSpec struct {
	InheritEnv []string
	ExtraEnv   map[string]string
}

type DoctorSpec struct {
	Short       string
	Binary      string
	VersionArgs []string
	InheritEnv  []string
	EnvVars     []doctor.EnvVar
	ExtraEnv    map[string]string
}

func Run(args []string, spec Spec) int {
	cmd := NewRootCommand(spec)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		_, _ = fmt.Fprintln(writerOrDiscard(spec.Stderr), err)
		return 1
	}
	return 0
}

func NewRootCommand(spec Spec) *cobra.Command {
	stdout := writerOrDiscard(spec.Stdout)
	stderr := writerOrDiscard(spec.Stderr)
	var runtimeBinary string
	var runtimeWorkDir string
	var runtimeArgs []string

	cmd := &cobra.Command{
		Use:           spec.Info.Name,
		Short:         spec.Short,
		SilenceErrors: true,
		SilenceUsage:  true,
		Version:       spec.Info.Version,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 0 {
				return fmt.Errorf("unknown argument: %s", args[0])
			}
			info := spec.Info
			var opts []acp.Option
			if runtimeBinary != "" {
				if runtimeWorkDir == "" {
					return fmt.Errorf("--runtime-workdir is required when --runtime-binary is set")
				}
				host := runtimehost.NewDeferred(cmd.Context(), runtimehost.Spec{
					Launch: runtimeproc.LaunchSpec{
						Binary:     runtimeBinary,
						Args:       runtimeArgs,
						WorkDir:    runtimeWorkDir,
						InheritEnv: append([]string(nil), spec.Runtime.InheritEnv...),
						ExtraEnv:   cloneStringMap(spec.Runtime.ExtraEnv),
					},
					ClientInfo: runtimeacp.ImplementationInfo{
						Name:    info.Name,
						Title:   info.Title,
						Version: info.Version,
					},
				})
				defer func() {
					_ = host.Close()
				}()
				opts = append(
					[]acp.Option{acp.WithInitializeHandler(host.Initialize)},
					runtimebridge.New(host).Options()...,
				)
			}
			server := acp.NewServer(info, opts...)
			if err := server.Serve(spec.Stdin, stdout); err != nil {
				return fmt.Errorf("adapter error: %w", err)
			}
			return nil
		},
	}
	cmd.SetIn(spec.Stdin)
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	cmd.Flags().StringVar(&runtimeBinary, "runtime-binary", "", "runtime executable to launch instead of scaffold handlers")
	cmd.Flags().StringVar(&runtimeWorkDir, "runtime-workdir", "", "absolute working directory for the runtime process")
	cmd.Flags().StringArrayVar(&runtimeArgs, "runtime-arg", nil, "argument to pass to the runtime process; repeat to pass multiple arguments")

	cmd.AddCommand(&cobra.Command{
		Use:           "version",
		Short:         "Print version information",
		SilenceErrors: true,
		SilenceUsage:  true,
		Args:          cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			_, _ = fmt.Fprintf(stdout, "%s %s\n", spec.Info.Name, spec.Info.Version)
		},
	})
	if spec.Doctor != nil {
		cmd.AddCommand(newDoctorCommand(spec.Info.Name, *spec.Doctor, stdout))
	}
	cmd.SetVersionTemplate(fmt.Sprintf("%s %s\n", spec.Info.Name, spec.Info.Version))
	return cmd
}

func newDoctorCommand(adapterName string, spec DoctorSpec, stdout io.Writer) *cobra.Command {
	var binary = spec.Binary
	var workDir string
	var jsonOutput bool
	versionArgs := append([]string(nil), spec.VersionArgs...)

	cmd := &cobra.Command{
		Use:           "doctor",
		Short:         spec.Short,
		SilenceErrors: true,
		SilenceUsage:  true,
		Args:          cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			report, err := doctor.Run(cmd.Context(), doctor.Spec{
				AdapterName: adapterName,
				Binary:      binary,
				VersionArgs: versionArgs,
				WorkDir:     workDir,
				InheritEnv:  append([]string(nil), spec.InheritEnv...),
				EnvVars:     append([]doctor.EnvVar(nil), spec.EnvVars...),
				ExtraEnv:    cloneStringMap(spec.ExtraEnv),
			})
			if jsonOutput {
				payload := struct {
					OK     bool          `json:"ok"`
					Error  string        `json:"error,omitempty"`
					Report doctor.Report `json:"report"`
				}{
					OK:     err == nil,
					Report: report,
				}
				if err != nil {
					payload.Error = err.Error()
				}
				encoder := json.NewEncoder(stdout)
				encoder.SetIndent("", "  ")
				if encodeErr := encoder.Encode(payload); encodeErr != nil {
					return encodeErr
				}
			} else {
				doctor.WriteReport(stdout, report, err)
			}
			if err != nil {
				return err
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&binary, "binary", spec.Binary, "runtime executable to probe")
	cmd.Flags().StringVar(&workDir, "workdir", "", "working directory for the probe (defaults to current directory)")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "write a JSON report")
	cmd.Flags().StringArrayVar(&versionArgs, "version-arg", spec.VersionArgs, "argument for the version probe; repeat to pass multiple arguments")
	return cmd
}

func writerOrDiscard(w io.Writer) io.Writer {
	if w == nil {
		return io.Discard
	}
	return w
}

func cloneStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	clone := make(map[string]string, len(values))
	for key, value := range values {
		clone[key] = value
	}
	return clone
}
