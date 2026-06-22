package acptest

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

const mcpEchoServerSource = `package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
)

type request struct {
	JSONRPC string          ` + "`json:\"jsonrpc\"`" + `
	ID      json.RawMessage ` + "`json:\"id,omitempty\"`" + `
	Method  string          ` + "`json:\"method,omitempty\"`" + `
	Params  json.RawMessage ` + "`json:\"params,omitempty\"`" + `
}

func main() {
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var req request
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			continue
		}
		if len(req.ID) == 0 {
			continue
		}
		switch req.Method {
		case "initialize":
			writeResponse(req.ID, map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities": map[string]any{
					"tools": map[string]any{},
				},
				"serverInfo": map[string]string{
					"name":    "acp-adapter-kit-echo-smoke",
					"version": "0.0.0",
				},
			})
		case "ping":
			writeResponse(req.ID, map[string]any{})
		case "tools/list":
			writeResponse(req.ID, map[string]any{
				"tools": []map[string]any{{
					"name":        "echo",
					"description": "Echo text back and record that the MCP tool was called.",
					"inputSchema": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"text": map[string]any{
								"type":        "string",
								"description": "Text to echo.",
							},
						},
						"required": []string{"text"},
					},
				}},
			})
		case "tools/call":
			handleToolCall(req.ID, req.Params)
		default:
			writeError(req.ID, -32601, "method not found")
		}
	}
	if err := scanner.Err(); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "scan MCP stdin: %v\n", err)
	}
}

func handleToolCall(id json.RawMessage, raw json.RawMessage) {
	var params struct {
		Name      string         ` + "`json:\"name\"`" + `
		Arguments map[string]any ` + "`json:\"arguments\"`" + `
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		writeError(id, -32602, "invalid tool call params")
		return
	}
	if params.Name != "echo" {
		writeError(id, -32602, "unknown tool")
		return
	}
	text, _ := params.Arguments["text"].(string)
	if text == "" {
		text = "empty"
	}
	if path := os.Getenv("ACP_MCP_ECHO_RECORD_PATH"); path != "" {
		_ = os.WriteFile(path, []byte(text), 0o600)
	}
	writeResponse(id, map[string]any{
		"content": []map[string]string{{
			"type": "text",
			"text": "mcp echo: " + text,
		}},
	})
}

func writeResponse(id json.RawMessage, result any) {
	writeEnvelope(map[string]any{
		"jsonrpc": "2.0",
		"id":      json.RawMessage(id),
		"result":  result,
	})
}

func writeError(id json.RawMessage, code int, message string) {
	writeEnvelope(map[string]any{
		"jsonrpc": "2.0",
		"id":      json.RawMessage(id),
		"error": map[string]any{
			"code":    code,
			"message": message,
		},
	})
}

func writeEnvelope(value any) {
	raw, err := json.Marshal(value)
	if err != nil {
		return
	}
	_, _ = os.Stdout.Write(append(raw, '\n'))
}
`

func BuildMCPStdioEchoServer(t testing.TB, dir string) string {
	t.Helper()
	if dir == "" {
		dir = t.TempDir()
	}
	sourcePath := filepath.Join(dir, "mcp-echo-server.go")
	if err := os.WriteFile(sourcePath, []byte(mcpEchoServerSource), 0o600); err != nil {
		t.Fatalf("write MCP echo server source: %v", err)
	}
	binaryName := "mcp-echo-server"
	if runtime.GOOS == "windows" {
		binaryName += ".exe"
	}
	binaryPath := filepath.Join(dir, binaryName)
	cmd := exec.Command("go", "build", "-o", binaryPath, sourcePath)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build MCP echo server: %v\n%s", err, string(out))
	}
	return binaryPath
}
