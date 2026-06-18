package adaptercli_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hecatehq/acp-adapter-kit/acp"
	"github.com/hecatehq/acp-adapter-kit/adaptercli"
	"github.com/hecatehq/acp-adapter-kit/doctor"
	adapterprocess "github.com/hecatehq/acp-adapter-kit/process"
)

func TestRunVersionFlagAndCommand(t *testing.T) {
	for _, args := range [][]string{{"--version"}, {"version"}} {
		var stdout bytes.Buffer
		code := adaptercli.Run(args, testSpec(nil, &stdout, nil))
		if code != 0 {
			t.Fatalf("Run(%v) returned %d, want 0", args, code)
		}
		if got, want := stdout.String(), "test-acp-adapter 1.2.3\n"; got != want {
			t.Fatalf("version output = %q, want %q", got, want)
		}
	}
}

func TestRunNoArgsServesScaffoldACP(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	input := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n")

	code := adaptercli.Run(nil, testSpec(input, &stdout, &stderr))
	if code != 0 {
		t.Fatalf("Run returned %d, want 0; stderr=%q", code, stderr.String())
	}

	var response map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v\n%s", err, stdout.String())
	}
	rawResult, err := json.Marshal(response["result"])
	if err != nil {
		t.Fatalf("marshal initialize result: %v", err)
	}
	var result struct {
		AgentInfo struct {
			Name    string `json:"name"`
			Title   string `json:"title"`
			Version string `json:"version"`
		} `json:"agentInfo"`
		AgentCapabilities struct {
			PromptCapabilities struct {
				Image           bool `json:"image"`
				EmbeddedContext bool `json:"embeddedContext"`
			} `json:"promptCapabilities"`
			MCPCapabilities struct {
				HTTP bool `json:"http"`
				SSE  bool `json:"sse,omitempty"`
			} `json:"mcpCapabilities"`
		} `json:"agentCapabilities"`
	}
	if err := json.Unmarshal(rawResult, &result); err != nil {
		t.Fatalf("decode initialize result: %v\n%s", err, rawResult)
	}
	if result.AgentInfo.Name != "test-acp-adapter" || result.AgentInfo.Title != "Test ACP Adapter" || result.AgentInfo.Version != "1.2.3" {
		t.Fatalf("agent info = %#v, want configured adapter metadata", result.AgentInfo)
	}
	if !result.AgentCapabilities.PromptCapabilities.Image || !result.AgentCapabilities.PromptCapabilities.EmbeddedContext {
		t.Fatalf("prompt capabilities = %#v, want image + embedded context", result.AgentCapabilities.PromptCapabilities)
	}
	if !result.AgentCapabilities.MCPCapabilities.HTTP || result.AgentCapabilities.MCPCapabilities.SSE {
		t.Fatalf("mcp capabilities = %#v, want HTTP only", result.AgentCapabilities.MCPCapabilities)
	}
}

func TestRuntimeBinaryRequiresRuntimeWorkdir(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := adaptercli.Run([]string{"--runtime-binary", os.Args[0]}, testSpec(nil, &stdout, &stderr))

	if code != 1 {
		t.Fatalf("Run returned %d, want 1", code)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if got := stderr.String(); !strings.Contains(got, "--runtime-workdir is required") {
		t.Fatalf("stderr = %q, want runtime-workdir error", got)
	}
}

func TestDoctorCommandWritesJSONReport(t *testing.T) {
	t.Setenv("GO_WANT_ADAPTERCLI_DOCTOR_HELPER", "1")
	t.Setenv("AGENT_API_KEY", "sk-agent-cli-secret")
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := adaptercli.Run([]string{
		"doctor",
		"--json",
		"--binary", os.Args[0],
		"--workdir", t.TempDir(),
		"--version-arg=-test.run=TestAdapterCLIDoctorHelper",
		"--version-arg=--",
		"--version-arg=version",
	}, testSpec(nil, &stdout, &stderr))

	if code != 0 {
		t.Fatalf("Run returned %d, want 0; stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	var payload struct {
		OK     bool          `json:"ok"`
		Error  string        `json:"error,omitempty"`
		Report doctor.Report `json:"report"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("decode doctor JSON: %v\n%s", err, stdout.String())
	}
	if !payload.OK || payload.Error != "" {
		t.Fatalf("payload status = ok:%v error:%q, want ok", payload.OK, payload.Error)
	}
	if payload.Report.AdapterName != "test-acp-adapter" || payload.Report.Binary != os.Args[0] {
		t.Fatalf("report metadata = %#v, want adapter name and override binary", payload.Report)
	}
	if !strings.Contains(payload.Report.VersionStdout, "fake-agent 1.2.3") {
		t.Fatalf("version stdout = %q, want fake version", payload.Report.VersionStdout)
	}
	if strings.Contains(payload.Report.VersionStdout, "sk-agent-cli-secret") {
		t.Fatalf("version stdout leaked secret: %q", payload.Report.VersionStdout)
	}
	if !strings.Contains(payload.Report.VersionStdout, adapterprocess.RedactedValue) {
		t.Fatalf("version stdout = %q, want redacted value", payload.Report.VersionStdout)
	}
}

func TestDoctorCommandWritesTextFailureAndReturnsError(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	missing := filepath.Join(t.TempDir(), "missing-agent")

	code := adaptercli.Run([]string{"doctor", "--binary", missing}, testSpec(nil, &stdout, &stderr))

	if code != 1 {
		t.Fatalf("Run returned %d, want 1", code)
	}
	if got := stdout.String(); !strings.Contains(got, "test-acp-adapter doctor: failed") || !strings.Contains(got, "binary: "+missing) {
		t.Fatalf("stdout = %q, want failed text report", got)
	}
	if got := stderr.String(); !strings.Contains(got, "find runtime binary") {
		t.Fatalf("stderr = %q, want command lookup error", got)
	}
}

func testSpec(stdin *strings.Reader, stdout *bytes.Buffer, stderr *bytes.Buffer) adaptercli.Spec {
	return adaptercli.Spec{
		Info: acp.AdapterInfo{
			Name:    "test-acp-adapter",
			Title:   "Test ACP Adapter",
			Version: "1.2.3",
			Capabilities: acp.Capabilities{
				Images:          true,
				EmbeddedContext: true,
				MCPHTTP:         true,
			},
		},
		Short:  "ACP adapter for tests",
		Stdin:  stdin,
		Stdout: stdout,
		Stderr: stderr,
		Doctor: &adaptercli.DoctorSpec{
			Short:       "Check the local test runtime boundary",
			Binary:      "test-agent",
			VersionArgs: []string{"--version"},
			InheritEnv:  []string{"GO_WANT_ADAPTERCLI_DOCTOR_HELPER"},
			EnvVars:     []doctor.EnvVar{{Name: "AGENT_API_KEY"}},
		},
	}
}

func TestAdapterCLIDoctorHelper(t *testing.T) {
	if os.Getenv("GO_WANT_ADAPTERCLI_DOCTOR_HELPER") != "1" {
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
	if args[sep+1] != "version" {
		os.Exit(2)
	}
	fmt.Printf("fake-agent 1.2.3 token=%s\n", os.Getenv("AGENT_API_KEY"))
	os.Exit(0)
}
