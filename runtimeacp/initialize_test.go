package runtimeacp_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/hecatehq/acp-adapter-kit/runtimeacp"
	"github.com/hecatehq/acp-adapter-kit/runtimejsonrpc"
	"github.com/hecatehq/acp-adapter-kit/runtimeproc"
)

func TestInitializeSendsClientInfoAndParsesResult(t *testing.T) {
	client := newInitializeClient(t, "ok")

	result, err := runtimeacp.Initialize(context.Background(), client, runtimeacp.InitializeParams{
		ProtocolVersion:    1,
		ClientInfo:         testClientInfo(),
		ClientCapabilities: testClientCapabilities(),
	})
	if err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if result.ProtocolVersion != 1 {
		t.Fatalf("ProtocolVersion = %d, want 1", result.ProtocolVersion)
	}
	if result.AgentInfo.Name != "fake-agent" {
		t.Fatalf("AgentInfo.Name = %q, want fake-agent", result.AgentInfo.Name)
	}
	if !result.AgentCapabilities.PromptCapabilities.Image {
		t.Fatal("PromptCapabilities.Image = false, want true")
	}
	if !result.AgentCapabilities.MCPCapabilities.ACP {
		t.Fatal("MCPCapabilities.ACP = false, want true")
	}
	if _, ok := result.AgentCapabilities.SessionCapabilities["list"]; !ok {
		t.Fatalf("SessionCapabilities = %#v, want list", result.AgentCapabilities.SessionCapabilities)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	if !strings.Contains(string(raw), `"x-result":{"kept":true}`) {
		t.Fatalf("marshaled result = %s, want x-result preserved", raw)
	}
}

func TestInitializeRejectsUnsupportedProtocolVersion(t *testing.T) {
	client := newInitializeClient(t, "version-mismatch")

	_, err := runtimeacp.Initialize(context.Background(), client, runtimeacp.InitializeParams{
		ProtocolVersion:    1,
		ClientInfo:         testClientInfo(),
		ClientCapabilities: testClientCapabilities(),
	})
	if err == nil || !errors.Is(err, runtimeacp.ErrUnsupportedProtocolVersion) {
		t.Fatalf("Initialize error = %v, want unsupported protocol version", err)
	}
}

func TestInitializePropagatesRuntimeRPCError(t *testing.T) {
	client := newInitializeClient(t, "rpc-error")

	_, err := runtimeacp.Initialize(context.Background(), client, runtimeacp.InitializeParams{
		ProtocolVersion:    1,
		ClientInfo:         testClientInfo(),
		ClientCapabilities: testClientCapabilities(),
	})
	var rpcErr *runtimejsonrpc.RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("Initialize error = %T %[1]v, want RPCError", err)
	}
	if rpcErr.Code != -32001 {
		t.Fatalf("RPCError.Code = %d, want -32001", rpcErr.Code)
	}
}

func testClientInfo() runtimeacp.ImplementationInfo {
	return runtimeacp.ImplementationInfo{
		Name:    "hecate-test",
		Title:   "Hecate Test",
		Version: "1.2.3",
	}
}

func testClientCapabilities() runtimeacp.ClientCapabilities {
	return runtimeacp.ClientCapabilities{
		Terminal: true,
		Auth:     &runtimeacp.AuthCapabilities{Terminal: true},
		FS: runtimeacp.FileSystemCapabilities{
			ReadTextFile:  true,
			WriteTextFile: true,
		},
	}
}

func newInitializeClient(t testing.TB, mode string) *runtimejsonrpc.Client {
	t.Helper()
	t.Setenv("GO_WANT_RUNTIMEACP_HELPER", "1")
	launcher := runtimeproc.NewLauncher(runtimeproc.Config{
		Binary:     os.Args[0],
		Args:       []string{"-test.run=TestRuntimeACPInitializeHelper", "--", mode},
		InheritEnv: []string{"GO_WANT_RUNTIMEACP_HELPER"},
	})
	client, err := runtimejsonrpc.Connect(context.Background(), runtimejsonrpc.ConnectSpec{
		Launcher: launcher,
		Launch: runtimeproc.LaunchSpec{
			WorkDir: t.TempDir(),
		},
	})
	if err != nil {
		t.Fatalf("Connect returned error: %v", err)
	}
	t.Cleanup(func() {
		_ = client.Kill()
		_ = client.Wait()
	})
	return client
}

func TestRuntimeACPInitializeHelper(t *testing.T) {
	if os.Getenv("GO_WANT_RUNTIMEACP_HELPER") != "1" {
		return
	}
	mode := "ok"
	if len(os.Args) > 0 {
		for i, arg := range os.Args {
			if arg == "--" && i+1 < len(os.Args) {
				mode = os.Args[i+1]
				break
			}
		}
	}
	decoder := json.NewDecoder(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	var req struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  struct {
			ProtocolVersion int `json:"protocolVersion"`
			ClientInfo      struct {
				Name    string `json:"name"`
				Title   string `json:"title"`
				Version string `json:"version"`
			} `json:"clientInfo"`
			ClientCapabilities struct {
				Auth struct {
					Terminal bool `json:"terminal"`
				} `json:"auth"`
				Terminal bool `json:"terminal"`
				FS       struct {
					ReadTextFile  bool `json:"readTextFile"`
					WriteTextFile bool `json:"writeTextFile"`
				} `json:"fs"`
			} `json:"clientCapabilities"`
		} `json:"params"`
	}
	if err := decoder.Decode(&req); err != nil {
		os.Exit(2)
	}
	if req.Method != "initialize" ||
		req.Params.ProtocolVersion != 1 ||
		req.Params.ClientInfo.Name != "hecate-test" ||
		!req.Params.ClientCapabilities.Auth.Terminal ||
		!req.Params.ClientCapabilities.Terminal ||
		!req.Params.ClientCapabilities.FS.ReadTextFile ||
		!req.Params.ClientCapabilities.FS.WriteTextFile {
		_ = encoder.Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      json.RawMessage(req.ID),
			"error": map[string]any{
				"code":    -32602,
				"message": fmt.Sprintf("bad initialize params: %+v", req.Params),
			},
		})
		os.Exit(0)
	}
	switch mode {
	case "ok":
		_ = encoder.Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      json.RawMessage(req.ID),
			"result": map[string]any{
				"protocolVersion": 1,
				"agentCapabilities": map[string]any{
					"loadSession": true,
					"promptCapabilities": map[string]any{
						"image":           true,
						"embeddedContext": true,
					},
					"mcpCapabilities": map[string]any{
						"acp":  true,
						"http": true,
					},
					"sessionCapabilities": map[string]any{
						"list": map[string]any{},
					},
				},
				"agentInfo": map[string]any{
					"name":    "fake-agent",
					"title":   "Fake Agent",
					"version": "9.9.9",
				},
				"authMethods": []map[string]any{},
				"x-result":    map[string]any{"kept": true},
			},
		})
	case "version-mismatch":
		_ = encoder.Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      json.RawMessage(req.ID),
			"result": map[string]any{
				"protocolVersion": 99,
				"agentCapabilities": map[string]any{
					"promptCapabilities": map[string]any{},
				},
				"agentInfo":   map[string]any{"name": "fake-agent"},
				"authMethods": []map[string]any{},
			},
		})
	case "rpc-error":
		_ = encoder.Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      json.RawMessage(req.ID),
			"error": map[string]any{
				"code":    -32001,
				"message": "auth required",
			},
		})
	default:
		os.Exit(2)
	}
	os.Exit(0)
}
