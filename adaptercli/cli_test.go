package adaptercli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hecatehq/acp-adapter-kit/acp"
	"github.com/hecatehq/acp-adapter-kit/adaptercli"
	"github.com/hecatehq/acp-adapter-kit/commandbridge"
	"github.com/hecatehq/acp-adapter-kit/doctor"
	adapterprocess "github.com/hecatehq/acp-adapter-kit/process"
	"github.com/hecatehq/acp-adapter-kit/runtimeacp"
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

func TestRuntimeFlagsUseConfiguredEnvironmentPolicy(t *testing.T) {
	t.Setenv("GO_WANT_ADAPTERCLI_RUNTIME_HELPER", "1")
	t.Setenv("AGENT_API_KEY", "sk-runtime")
	t.Setenv("AGENT_SECRET", "should-not-leak")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	input := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n")
	spec := testSpec(input, &stdout, &stderr)
	spec.Runtime = adaptercli.RuntimeSpec{
		InheritEnv: []string{"GO_WANT_ADAPTERCLI_RUNTIME_HELPER", "AGENT_API_KEY"},
		ExtraEnv:   map[string]string{"AGENT_HOME": "runtime-home"},
	}

	code := adaptercli.Run([]string{
		"--runtime-binary", os.Args[0],
		"--runtime-workdir", t.TempDir(),
		"--runtime-arg=-test.run=TestAdapterCLIRuntimeHelper",
		"--runtime-arg=--",
		"--runtime-arg=adaptercli-runtime-helper",
	}, spec)

	if code != 0 {
		t.Fatalf("Run returned %d, want 0; stderr=%q stdout=%q", code, stderr.String(), stdout.String())
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
			Name string `json:"name"`
		} `json:"agentInfo"`
	}
	if err := json.Unmarshal(rawResult, &result); err != nil {
		t.Fatalf("decode initialize result: %v\n%s", err, rawResult)
	}
	if result.AgentInfo.Name != "runtime-env-helper" {
		t.Fatalf("agent name = %q, want runtime-env-helper", result.AgentInfo.Name)
	}
}

func TestCommandBridgeServesSessionMethodsWithoutRuntimeFlags(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	input := strings.NewReader(strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":"/tmp/work"}}`,
		`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"session-1","prompt":[{"type":"text","text":"hello"}]}}`,
	}, "\n") + "\n")
	spec := testSpec(input, &stdout, &stderr)
	spec.Command = &commandbridge.Spec{
		NewID: func() string { return "session-1" },
		BuildPrompt: func(session commandbridge.Session, params runtimeacp.PromptParams) (adapterprocess.Spec, error) {
			text, err := commandbridge.RequirePromptText(params)
			if err != nil {
				return adapterprocess.Spec{}, err
			}
			return adapterprocess.Spec{Command: "test-agent", Dir: session.CWD, Args: []string{text}}, nil
		},
		Runner: commandbridge.RunnerFunc(func(_ context.Context, spec adapterprocess.Spec) (adapterprocess.Result, error) {
			if spec.Command != "test-agent" || spec.Dir != "/tmp/work" || strings.Join(spec.Args, " ") != "hello" {
				t.Fatalf("process spec = %#v, want command prompt", spec)
			}
			return adapterprocess.Result{Stdout: []byte("hi from command bridge")}, nil
		}),
	}

	code := adaptercli.Run(nil, spec)

	if code != 0 {
		t.Fatalf("Run returned %d, want 0; stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	responses := decodeResponses(t, stdout.Bytes())
	if len(responses) != 5 {
		t.Fatalf("got %d responses, want session/new + tool start + chunk + tool finish + prompt\n%s", len(responses), stdout.String())
	}
	start := decodeCLIUpdate(t, responses[1])
	if start.Update.SessionUpdate != "tool_call" ||
		start.Update.ToolCallID == "" ||
		start.Update.Status != "in_progress" ||
		start.Update.RawInput["command"] != "test-agent hello" ||
		start.Update.RawInput["cwd"] != "/tmp/work" {
		t.Fatalf("tool start = %#v, want command metadata", start)
	}
	chunk := decodeCLIUpdate(t, responses[2])
	var chunkContent struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(chunk.Update.Content, &chunkContent); err != nil {
		t.Fatalf("decode chunk content: %v\n%s", err, string(chunk.Update.Content))
	}
	if chunk.Update.SessionUpdate != "agent_message_chunk" ||
		chunkContent.Text != "hi from command bridge" {
		t.Fatalf("chunk = %#v, want assistant output", chunk)
	}
	finish := decodeCLIUpdate(t, responses[3])
	if finish.Update.SessionUpdate != "tool_call_update" ||
		finish.Update.ToolCallID != start.Update.ToolCallID ||
		finish.Update.Status != "completed" {
		t.Fatalf("tool finish = %#v, want completed command", finish)
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

type cliResponse struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
}

func decodeResponses(t testing.TB, raw []byte) []cliResponse {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	out := make([]cliResponse, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var response cliResponse
		if err := json.Unmarshal([]byte(line), &response); err != nil {
			t.Fatalf("decode response %q: %v", line, err)
		}
		out = append(out, response)
	}
	return out
}

type cliSessionUpdate struct {
	SessionID string `json:"sessionId"`
	Update    struct {
		SessionUpdate string          `json:"sessionUpdate"`
		ToolCallID    string          `json:"toolCallId"`
		Status        string          `json:"status"`
		RawInput      map[string]any  `json:"rawInput"`
		Content       json.RawMessage `json:"content"`
	} `json:"update"`
}

func decodeCLIUpdate(t testing.TB, response cliResponse) cliSessionUpdate {
	t.Helper()
	if response.Method != "session/update" {
		t.Fatalf("response method = %q, want session/update", response.Method)
	}
	var update cliSessionUpdate
	if err := json.Unmarshal(response.Params, &update); err != nil {
		t.Fatalf("decode update: %v\n%s", err, string(response.Params))
	}
	return update
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

func TestAdapterCLIRuntimeHelper(t *testing.T) {
	if os.Getenv("GO_WANT_ADAPTERCLI_RUNTIME_HELPER") != "1" || !hasArg(os.Args, "adaptercli-runtime-helper") {
		return
	}
	if os.Getenv("AGENT_API_KEY") != "sk-runtime" {
		fmt.Fprintf(os.Stderr, "AGENT_API_KEY=%q\n", os.Getenv("AGENT_API_KEY"))
		os.Exit(3)
	}
	if os.Getenv("AGENT_HOME") != "runtime-home" {
		fmt.Fprintf(os.Stderr, "AGENT_HOME=%q\n", os.Getenv("AGENT_HOME"))
		os.Exit(4)
	}
	if os.Getenv("AGENT_SECRET") != "" {
		fmt.Fprintf(os.Stderr, "AGENT_SECRET leaked\n")
		os.Exit(5)
	}
	decoder := json.NewDecoder(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for {
		var msg struct {
			ID     json.RawMessage `json:"id,omitempty"`
			Method string          `json:"method"`
		}
		if err := decoder.Decode(&msg); err != nil {
			return
		}
		switch msg.Method {
		case "initialize":
			_ = encoder.Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      json.RawMessage(msg.ID),
				"result": map[string]any{
					"protocolVersion": 1,
					"agentInfo":       map[string]any{"name": "runtime-env-helper"},
					"agentCapabilities": map[string]any{
						"loadSession": true,
					},
				},
			})
		default:
			_ = encoder.Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      json.RawMessage(msg.ID),
				"error":   map[string]any{"code": -32601, "message": "not found"},
			})
		}
	}
}

func hasArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}
