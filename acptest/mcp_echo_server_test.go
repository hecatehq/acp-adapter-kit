package acptest_test

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"testing"

	"github.com/hecatehq/acp-adapter-kit/acptest"
)

func TestBuildMCPStdioEchoServer(t *testing.T) {
	dir := t.TempDir()
	binary := acptest.BuildMCPStdioEchoServer(t, dir)
	recordPath := dir + "/record.txt"
	cmd := exec.Command(binary)
	cmd.Env = append(os.Environ(), "ACP_MCP_ECHO_RECORD_PATH="+recordPath)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start MCP echo server: %v", err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	decoder := json.NewDecoder(stdout)

	writeMCPRequest(t, stdin, "init", "initialize", map[string]any{})
	var initResp map[string]any
	if err := decoder.Decode(&initResp); err != nil {
		t.Fatalf("decode initialize response: %v", err)
	}
	if initResp["id"] != "init" {
		t.Fatalf("initialize response = %#v, want id init", initResp)
	}

	writeMCPRequest(t, stdin, "tools", "tools/list", map[string]any{})
	var listResp struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := decoder.Decode(&listResp); err != nil {
		t.Fatalf("decode tools/list response: %v", err)
	}
	if len(listResp.Result.Tools) != 1 || listResp.Result.Tools[0].Name != "echo" {
		t.Fatalf("tools/list = %#v, want echo tool", listResp.Result.Tools)
	}

	writeMCPRequest(t, stdin, "call", "tools/call", map[string]any{
		"name":      "echo",
		"arguments": map[string]any{"text": "hello from test"},
	})
	var callResp struct {
		Result struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := decoder.Decode(&callResp); err != nil {
		t.Fatalf("decode tools/call response: %v", err)
	}
	if len(callResp.Result.Content) != 1 || callResp.Result.Content[0].Text != "mcp echo: hello from test" {
		t.Fatalf("tools/call = %#v, want echoed text", callResp.Result.Content)
	}
	raw, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatalf("read MCP echo record: %v", err)
	}
	if string(raw) != "hello from test" {
		t.Fatalf("record = %q, want tool call text", string(raw))
	}
}

func writeMCPRequest(t testing.TB, stdin io.Writer, id, method string, params any) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	})
	if err != nil {
		t.Fatalf("marshal MCP request: %v", err)
	}
	if _, err := stdin.Write(append(raw, '\n')); err != nil {
		t.Fatalf("write MCP request: %v", err)
	}
}
