package runtimehost_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/hecatehq/acp-adapter-kit/acp"
	"github.com/hecatehq/acp-adapter-kit/acptest"
	"github.com/hecatehq/acp-adapter-kit/runtimeacp"
	"github.com/hecatehq/acp-adapter-kit/runtimehost"
	"github.com/hecatehq/acp-adapter-kit/runtimeproc"
)

func TestStartInitializesRuntimeAndProxiesSessionMethods(t *testing.T) {
	host := newHelperHost(t, "happy")

	if host.InitializeResult().AgentInfo.Name != "helper-runtime" {
		t.Fatalf("agent name = %q, want helper-runtime", host.InitializeResult().AgentInfo.Name)
	}
	if !host.InitializeResult().AgentCapabilities.PromptCapabilities.Image {
		t.Fatal("image prompt capability = false, want true")
	}
	rawInitialize, err := json.Marshal(host.InitializeResult())
	if err != nil {
		t.Fatalf("marshal initialize result: %v", err)
	}
	if !strings.Contains(string(rawInitialize), `"x-runtime":{"kept":true}`) {
		t.Fatalf("initialize result = %s, want x-runtime preserved", rawInitialize)
	}

	client := acptest.NewClient(t, acp.NewServer(acp.AdapterInfo{Name: "test-adapter"}, host.Options()...))
	newSession := client.Request("session/new", map[string]any{"cwd": "/tmp/project"})
	var session struct {
		SessionID string `json:"sessionId"`
	}
	newSession.ResultInto(t, &session)
	if session.SessionID != "runtime-session" {
		t.Fatalf("sessionId = %q, want runtime-session", session.SessionID)
	}

	envelopes := client.Send(map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "session/prompt",
		"params": map[string]any{
			"sessionId": session.SessionID,
			"prompt": []map[string]string{{
				"type": "text",
				"text": "hello",
			}},
		},
	})
	if len(envelopes) != 2 {
		t.Fatalf("got %d envelopes, want update notification + prompt response", len(envelopes))
	}
	if envelopes[0].Method != "session/update" {
		t.Fatalf("first envelope method = %q, want session/update", envelopes[0].Method)
	}
	var promptResult struct {
		StopReason string `json:"stopReason"`
	}
	envelopes[1].ResultInto(t, &promptResult)
	if promptResult.StopReason != string(runtimeacp.StopReasonEndTurn) {
		t.Fatalf("stopReason = %q, want end_turn", promptResult.StopReason)
	}
}

func TestStartRejectsUnsupportedProtocolVersion(t *testing.T) {
	t.Setenv("GO_WANT_RUNTIMEHOST_HELPER", "1")
	launcher := runtimeproc.NewLauncher(runtimeproc.Config{
		Binary:     os.Args[0],
		Args:       []string{"-test.run=TestRuntimeHostHelper", "--", "unsupported-protocol"},
		InheritEnv: []string{"GO_WANT_RUNTIMEHOST_HELPER"},
	})

	host, err := runtimehost.Start(context.Background(), runtimehost.Spec{
		Launcher: launcher,
		Launch:   runtimeproc.LaunchSpec{WorkDir: t.TempDir()},
		ClientInfo: runtimeacp.ImplementationInfo{
			Name:    "test-adapter",
			Title:   "Test Adapter",
			Version: "test-version",
		},
		ClientCapabilities: runtimeacp.ClientCapabilities{
			Terminal: true,
			Auth:     &runtimeacp.AuthCapabilities{Terminal: true},
			FS: runtimeacp.FileSystemCapabilities{
				ReadTextFile:  true,
				WriteTextFile: true,
			},
		},
	})
	if host != nil {
		t.Fatalf("host = %#v, want nil", host)
	}
	if err == nil || !strings.Contains(err.Error(), "unsupported ACP protocol version") {
		t.Fatalf("Start error = %v, want unsupported protocol version", err)
	}
}

func newHelperHost(t testing.TB, mode string) *runtimehost.Host {
	t.Helper()
	t.Setenv("GO_WANT_RUNTIMEHOST_HELPER", "1")
	launcher := runtimeproc.NewLauncher(runtimeproc.Config{
		Binary:     os.Args[0],
		Args:       []string{"-test.run=TestRuntimeHostHelper", "--", mode},
		InheritEnv: []string{"GO_WANT_RUNTIMEHOST_HELPER"},
	})
	host, err := runtimehost.Start(context.Background(), runtimehost.Spec{
		Launcher: launcher,
		Launch:   runtimeproc.LaunchSpec{WorkDir: t.TempDir()},
		ClientInfo: runtimeacp.ImplementationInfo{
			Name:    "test-adapter",
			Title:   "Test Adapter",
			Version: "test-version",
		},
		ClientCapabilities: runtimeacp.ClientCapabilities{
			Terminal: true,
			Auth:     &runtimeacp.AuthCapabilities{Terminal: true},
			FS: runtimeacp.FileSystemCapabilities{
				ReadTextFile:  true,
				WriteTextFile: true,
			},
		},
	})
	if err != nil {
		t.Fatalf("Start returned error: %v", err)
	}
	t.Cleanup(func() {
		_ = host.Close()
	})
	return host
}

func TestRuntimeHostHelper(t *testing.T) {
	if os.Getenv("GO_WANT_RUNTIMEHOST_HELPER") != "1" {
		return
	}
	mode := "happy"
	if sep := indexArg(os.Args, "--"); sep >= 0 && sep+1 < len(os.Args) {
		mode = os.Args[sep+1]
	}
	decoder := json.NewDecoder(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for {
		var msg struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id,omitempty"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params,omitempty"`
		}
		if err := decoder.Decode(&msg); err != nil {
			return
		}
		if msg.Method == "initialize" {
			writeInitializeResponse(encoder, msg.ID, msg.Params, mode)
			continue
		}
		switch msg.Method {
		case "session/new":
			_ = encoder.Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      json.RawMessage(msg.ID),
				"result":  map[string]any{"sessionId": "runtime-session"},
			})
		case "session/prompt":
			_ = encoder.Encode(map[string]any{
				"jsonrpc": "2.0",
				"method":  "session/update",
				"params": map[string]any{
					"sessionId": "runtime-session",
					"update": map[string]any{
						"sessionUpdate": "agent_message_chunk",
						"content":       map[string]any{"type": "text", "text": "hello from child"},
					},
				},
			})
			_ = encoder.Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      json.RawMessage(msg.ID),
				"result":  map[string]any{"stopReason": "end_turn"},
			})
		case "session/close":
			_ = encoder.Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      json.RawMessage(msg.ID),
				"result":  map[string]any{},
			})
		case "session/cancel":
			if len(msg.ID) != 0 {
				_ = encoder.Encode(map[string]any{
					"jsonrpc": "2.0",
					"id":      json.RawMessage(msg.ID),
					"result":  map[string]any{"cancelled": true},
				})
			}
		default:
			_ = encoder.Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      json.RawMessage(msg.ID),
				"error": map[string]any{
					"code":    -32601,
					"message": fmt.Sprintf("method %s not found", msg.Method),
				},
			})
		}
	}
}

func writeInitializeResponse(encoder *json.Encoder, id json.RawMessage, params json.RawMessage, mode string) {
	var req runtimeacp.InitializeParams
	if err := json.Unmarshal(params, &req); err != nil {
		_ = encoder.Encode(rpcError(id, -32602, "invalid initialize params", err.Error()))
		return
	}
	if req.ProtocolVersion != runtimeacp.ProtocolVersion ||
		req.ClientInfo.Name != "test-adapter" ||
		req.ClientInfo.Title != "Test Adapter" ||
		req.ClientInfo.Version != "test-version" ||
		req.ClientCapabilities.Auth == nil ||
		!req.ClientCapabilities.Auth.Terminal ||
		!req.ClientCapabilities.Terminal ||
		!req.ClientCapabilities.FS.ReadTextFile ||
		!req.ClientCapabilities.FS.WriteTextFile {
		_ = encoder.Encode(rpcError(id, -32050, "unexpected initialize params", string(params)))
		return
	}
	version := runtimeacp.ProtocolVersion
	if mode == "unsupported-protocol" {
		version = runtimeacp.ProtocolVersion + 1
	}
	_ = encoder.Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      json.RawMessage(id),
		"result": map[string]any{
			"protocolVersion": version,
			"agentInfo": map[string]any{
				"name":    "helper-runtime",
				"title":   "Helper Runtime",
				"version": "test",
			},
			"agentCapabilities": map[string]any{
				"promptCapabilities": map[string]any{"image": true},
			},
			"x-runtime": map[string]any{"kept": true},
		},
	})
}

func rpcError(id json.RawMessage, code int, message string, data any) map[string]any {
	return map[string]any{
		"jsonrpc": "2.0",
		"id":      json.RawMessage(id),
		"error": map[string]any{
			"code":    code,
			"message": message,
			"data":    data,
		},
	}
}

func indexArg(args []string, want string) int {
	for i, arg := range args {
		if arg == want {
			return i
		}
	}
	return -1
}
